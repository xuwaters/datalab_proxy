package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"hash"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/redis"
	"github.com/gin-gonic/gin"
	"github.com/satori/go.uuid"
)

type APIService struct {
	config *ServiceConfig
	engine *gin.Engine
}

func NewAPIService(config *ServiceConfig) *APIService {
	return &APIService{
		config: config,
		engine: nil,
	}
}

func (service *APIService) Run(ctx context.Context, debugMode bool) (err error) {
	service.setupGinMode(debugMode)
	if err = service.setupEngine(); err != nil {
		return
	}
	log.Printf("APIService listening at: %s", service.config.Listen)
	if !service.config.TLSEnabled {
		err = service.engine.Run(service.config.Listen)
	} else {
		err = service.ListenAndServeTLS(ctx)
	}
	log.Printf("Service Run error = %v", err)
	return
}

func (service *APIService) ListenAndServeTLS(ctx context.Context) (err error) {
	log.Printf("Listening and serving HTTPS on %s", service.config.Listen)

	var (
		reloader *CertReloader
		config   *tls.Config
		server   *http.Server
	)

	reloader, err = NewCertReloader(ctx, service.config.TLSCertFile, service.config.TLSKeyFile)

	if err != nil {
		log.Fatalf("load certificate and key failure: %v", err)
		return
	}

	config = &tls.Config{
		GetCertificate: reloader.CreateGetCertificateFunc(),
	}

	server = &http.Server{
		Addr:      service.config.Listen,
		Handler:   service.engine,
		TLSConfig: config,
	}

	err = server.ListenAndServeTLS("", "")

	return
}

func (service *APIService) setupGinMode(debugMode bool) {
	var mode = os.Getenv(gin.ENV_GIN_MODE)
	if mode == "" {
		mode = gin.ReleaseMode
	}
	if debugMode {
		mode = gin.DebugMode
	}
	log.Printf("APIService run mode = %s", mode)
	gin.SetMode(mode)
}

func (service *APIService) setupEngine() (err error) {
	var (
		store  sessions.Store
		engine *gin.Engine
	)

	store, err = redis.NewStore(
		128, "tcp", service.config.RedisStoreAddress, "",
		[]byte(service.config.CookieKey),
	)
	if err != nil {
		return
	}

	engine = gin.Default()
	engine.Use(sessions.Sessions("datalab_session", store))

	engine.GET("/_healthcheck", service.onRequestHealthCheck)
	engine.POST("/datalab/start", service.onRequestDatalabStart)
	engine.GET("/datalab/view/:nonce/*destination", service.onRequestDatalabView)
	engine.GET("/datalab/static/:nonce/*destination", service.onRequestDatalabFiles)
	engine.NoRoute(service.onRequestDatalabProxy)

	service.engine = engine

	return
}

func (service *APIService) sendErrorResponse(c *gin.Context, code string, message string) {
	c.JSON(http.StatusOK, gin.H{
		"code":    code,
		"message": message,
	})
}

func (service *APIService) sendOKResponse(c *gin.Context, result gin.H) {
	c.JSON(http.StatusOK, gin.H{
		"code":   "OK",
		"result": result,
	})
}

func (service *APIService) onRequestHealthCheck(c *gin.Context) {
	var key = c.Query("key")
	if service.config.HealthCheckKey == key {
		c.JSON(http.StatusOK, gin.H{
			"code":   "OK",
			"result": "",
		})
	} else {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":    "ERR_INVALID_REQUEST",
			"message": "",
		})
	}
}

func (service *APIService) onRequestDatalabStart(c *gin.Context) {
	var (
		err error
	)

	var timestampParam = strings.TrimSpace(c.Query("ts"))
	var filename = c.Query("filename")

	if !service.config.SignDisabled {
		if timestampParam == "" {
			log.Printf("timestamp empty")
			c.JSON(http.StatusOK, gin.H{
				"code": "ERR_INVALID_PARAMS",
			})
			return
		}
		var timestamp int
		timestamp, _ = strconv.Atoi(timestampParam)
		var currTimestamp = int(time.Now().Unix())
		var diff = timestamp - currTimestamp
		if diff < -service.config.SignTTL && diff > service.config.SignTTL {
			log.Printf("timestamp expired: current = %d, got = %d, diff = %d", currTimestamp, timestamp, diff)
			c.JSON(http.StatusOK, gin.H{
				"code": "ERR_INVALID_PARAMS",
			})
			return
		}

		var signHeader = c.GetHeader("X-SIGN")
		var expectedSignature = service.calculateSignature(timestampParam, filename)
		if signHeader != expectedSignature {
			log.Printf("signature error: expect = %s, got = %s", expectedSignature, signHeader)
			c.JSON(http.StatusOK, gin.H{
				"code": "ERR_INVALID_REQUEST",
			})
			return
		}
	}

	filename = strings.TrimSpace(filename)
	if filename == "" {
		filename = "default.ipynb"
	}
	filename = regexp.MustCompile("(^[.]+)|([^0-9a-zA-Z-_./]+)").ReplaceAllString(filename, "")
	filename = strings.Replace(filename, "/", "_", -1)

	if c.Request.ContentLength > service.config.MaxContentLength {
		log.Printf("request content length too large: %d", c.Request.ContentLength)
		service.sendErrorResponse(c, "ERR_CONTENT_TOO_LARGE", "")
		return
	}

	var content []byte
	content, err = ioutil.ReadAll(c.Request.Body)
	if err != nil {
		log.Printf("read content request failure: %v", err)
		service.sendErrorResponse(c, "ERR_READ_CONTENT", "")
		return
	}

	var (
		containerID    string
		hookAfterStart HookCallback
	)
	var nonce = uuid.NewV4().String()
	containerID, hookAfterStart, err = datalabPool.AllocateInstance(nonce, filename, content)
	if err != nil {
		log.Printf("out of datalab instances")
		service.sendErrorResponse(c, "ERR_OUT_OF_RESOURCE", "")
		return
	}

	if hookAfterStart != nil {
		err = hookAfterStart()
		if err != nil {
			datalabPool.KillInstance(containerID)
			log.Printf("hook afterstart failure: container %v, err= %v", containerID, err)
			service.sendErrorResponse(c, "ERR_POST_SCRIPT", "")
			return
		}
	}

	log.Printf("start datalab success: container_id = %s, nonce = %s", containerID, nonce)
	service.sendOKResponse(c, gin.H{
		"nonce": nonce,
	})

	return
}

func (service *APIService) calculateSignature(strList ...string) string {
	var hasher hash.Hash
	hasher = sha256.New()
	hasher.Write([]byte(service.config.SignKey))
	for _, str := range strList {
		hasher.Write([]byte(str))
	}
	var signBytes []byte
	signBytes = hasher.Sum(nil)
	return strings.ToLower(hex.EncodeToString(signBytes))
}

// redirect and set session cookie
func (service *APIService) onRequestDatalabView(c *gin.Context) {
	var (
		err         error
		nonce       string
		destination string
		sess        sessions.Session
		containerID string
	)
	sess = sessions.Default(c)
	nonce = c.Param("nonce")
	destination = c.Param("destination")
	if destination == "" {
		destination = "/"
	}
	log.Printf("nonce id = %v, destination = %v", nonce, destination)

	var checkRunning = false
	if destination == "/" {
		checkRunning = true
	}
	containerID, err = datalabPool.GetInstanceByNonce(nonce, checkRunning)
	if err != nil {
		log.Printf("nonce not found: %v", err)
		c.JSON(http.StatusNotFound, "not found")
		return
	}

	sess.Set("container_id", containerID)
	if err = sess.Save(); err != nil {
		log.Printf("save session data failure: %v", err)
		c.JSON(http.StatusInternalServerError, "internal server error")
		return
	}

	c.Redirect(302, destination)
}

// fetch files directly
func (service *APIService) onRequestDatalabFiles(c *gin.Context) {
	var (
		err         error
		nonce       string
		destination string
		containerID string
	)
	nonce = c.Param("nonce")
	destination = c.Param("destination")
	log.Printf("nonce id = %v, destination = %v", nonce, destination)

	if destination == "" || destination == "/" {
		c.Redirect(302, "/")
		return
	}

	containerID, err = datalabPool.GetInstanceByNonce(nonce, false)
	if err != nil {
		log.Printf("nonce not found: %v", err)
		c.JSON(http.StatusNotFound, "not found")
		return
	}

	var sess = sessions.Default(c)
	sess.Set("container_id", containerID)
	if err = sess.Save(); err != nil {
		log.Printf("save session data failure: %v", err)
		c.JSON(http.StatusInternalServerError, "internal server error")
		return
	}

	var instAddress string
	instAddress, err = datalabPool.GetInstanceAddressByContainerID(containerID)
	if err != nil {
		log.Printf("no such container: %s, path = %s", containerID, c.Request.RequestURI)
		c.JSON(http.StatusNotFound, "")
		return
	}

	var targetAddress = instAddress
	var targetURI *url.URL
	targetURI, err = url.Parse(targetAddress)
	if err != nil {
		log.Printf("parsing target url: %s, error = %v", targetAddress, err)
		c.JSON(http.StatusNotFound, "")
		return
	}
	log.Printf("proxy '%s' to backend: %s, path = %s", containerID, targetURI, c.Request.RequestURI)

	targetQuery := targetURI.RawQuery
	director := func(req *http.Request) {
		req.URL.Scheme = targetURI.Scheme
		req.URL.Host = targetURI.Host
		req.URL.Path = destination
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		req.Header.Set("Host", req.Host)
	}
	var httpProxy = &httputil.ReverseProxy{Director: director}
	httpProxy.ServeHTTP(c.Writer, c.Request)
}

func (service *APIService) onRequestDatalabProxy(c *gin.Context) {
	var (
		err           error
		sess          sessions.Session
		containerID   string
		instAddress   string
		targetAddress string
		targetURI     *url.URL
	)

	sess = sessions.Default(c)
	var ret = sess.Get("container_id")
	if ret == nil {
		log.Printf("session not found: %s, path = %s", c.Request.RemoteAddr, c.Request.RequestURI)
		c.JSON(http.StatusNotFound, "")
		return
	}
	containerID = ret.(string)

	instAddress, err = datalabPool.GetInstanceAddressByContainerID(containerID)
	if err != nil {
		log.Printf("no such container: %s, path = %s", containerID, c.Request.RequestURI)
		c.JSON(http.StatusNotFound, "")
		return
	}

	service.checkAndUpdateDatalabTTL(containerID, c)

	targetAddress = instAddress
	targetURI, err = url.Parse(targetAddress)
	if err != nil {
		log.Printf("parsing target url: %s, error = %v", targetAddress, err)
		c.JSON(http.StatusNotFound, "")
		return
	}
	log.Printf("proxy '%s' to backend: %s, path = %s", containerID, targetURI, c.Request.RequestURI)

	var upgradeHeader = c.Request.Header.Get("Upgrade")
	if upgradeHeader == "websocket" {
		targetURI.Scheme = "ws"
		log.Printf("websocket connection for %s: %s, path = %s", containerID, targetURI, c.Request.RequestURI)
		var websocketProxy = NewSingleHostReverseProxy(targetURI)
		websocketProxy.ServeHTTP(c.Writer, c.Request)
	} else {
		var httpProxy = NewSingleHostReverseProxyKeepHost(targetURI)
		httpProxy.ServeHTTP(c.Writer, c.Request)
	}
}

func (service *APIService) checkAndUpdateDatalabTTL(containerID string, c *gin.Context) {
	// check request: _timeout?reset=true
	if c.Request.URL.Path == "/_timeout" {
		if reset, found := c.GetQuery("reset"); found {
			if reset == "true" {
				// use is on page and typing
				datalabPool.UpdateInstanceUserEvent(containerID, UserEventIdleReset)
			}
		} else {
			// user is inactive but browser is still open
			datalabPool.UpdateInstanceUserEvent(containerID, UserEventKeepAlive)
		}
	}
}

func NewSingleHostReverseProxyKeepHost(target *url.URL) *httputil.ReverseProxy {
	targetQuery := target.RawQuery
	director := func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
		if targetQuery == "" || req.URL.RawQuery == "" {
			req.URL.RawQuery = targetQuery + req.URL.RawQuery
		} else {
			req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
		}
		if _, ok := req.Header["User-Agent"]; !ok {
			// explicitly disable User-Agent so it's not set to default value
			req.Header.Set("User-Agent", "")
		}
		req.Header.Set("Host", req.Host)
	}
	return &httputil.ReverseProxy{Director: director}
}
