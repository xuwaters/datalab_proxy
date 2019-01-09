package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v2"

	"github.com/fsouza/go-dockerclient"
	"github.com/go-redis/redis"
)

var (
	ErrDockerAlreadyInitialized    = fmt.Errorf("docker client already initialized")
	ErrRedisAlreadyInitialized     = fmt.Errorf("redis client already initialized")
	ErrDatalabInstanceNotAvailable = fmt.Errorf("instance not available")
	ErrDatalabScriptInvalid        = fmt.Errorf("datalab script error")
	ErrDatalabInstanceNotRunning   = fmt.Errorf("instance not running")
)

// store
// nonce:${nonce} -> ${cid} ${ttl}
// container:${cid} -> { ${event_name} -> ${timestamp} }

type DatalabPool struct {
	mutex        *sync.RWMutex
	config       *DatalabConfig
	dockerEvents chan *docker.APIEvents
	dockerClient *docker.Client
	redisClient  *redis.Client
	store        DataStore
}

func NewDatalabPool(config *DatalabConfig) *DatalabPool {
	return &DatalabPool{
		config:       config,
		mutex:        &sync.RWMutex{},
		dockerEvents: make(chan *docker.APIEvents, 1),
		dockerClient: nil,
		redisClient:  nil,
	}
}

func (pool *DatalabPool) Initialize(ctx context.Context) (err error) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	if err = pool.initDocker(ctx, pool.config.DockerEndpoint); err != nil {
		return
	}

	if err = pool.initRedis(ctx, pool.config.RedisCacheURL); err != nil {
		return
	}

	pool.store = NewRedisDataStore(pool.redisClient, "datalab")
	log.Printf("datalab pool initialized")

	return
}

func (pool *DatalabPool) initDocker(ctx context.Context, endpoint string) (err error) {
	if pool.dockerClient != nil {
		return ErrDockerAlreadyInitialized
	}
	pool.dockerClient, err = docker.NewClient(endpoint)
	if err != nil {
		pool.dockerClient = nil
		return
	}

	go pool.processContainerCleanup(ctx)
	go pool.processDockerEvents(ctx)

	pool.dockerClient.AddEventListener(pool.dockerEvents)

	log.Printf("docker client initialized: %s", endpoint)
	return
}

func (pool *DatalabPool) initRedis(ctx context.Context, endpoint string) (err error) {
	var options *redis.Options
	if pool.redisClient != nil {
		return ErrRedisAlreadyInitialized
	}
	if options, err = redis.ParseURL(endpoint); err != nil {
		return
	}
	pool.redisClient = redis.NewClient(options).WithContext(ctx)
	log.Printf("redis client initialized: %s", endpoint)
	return
}

func (pool *DatalabPool) processContainerCleanup(ctx context.Context) {
	var ticker = time.NewTicker(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			log.Printf("DONE: container cleanup goroutine")
			ticker.Stop()
			return

		case <-ticker.C:
			// check idle containers
			pool.checkIdleContainers()
		}
	}
}

func (pool *DatalabPool) checkIdleContainers() {
	var prefix = "container"
	var pattern = fmt.Sprintf("%s:*", prefix)
	var cidList []string
	var err error
	cidList, err = pool.store.Keys(pattern)
	if err != nil {
		log.Printf("fetch keys failure, pattern = %s, error = %v", pattern, err)
		return
	}
	for _, containerKey := range cidList {
		var containerID = containerKey[len(prefix)+1:]
		var data map[string]string
		data, err = pool.store.HGetAll(pool.makeContainerKey(containerID))
		if err != nil || len(data) == 0 {
			log.Printf("get container data failure: %s, err = %v, data = %v", containerID, err, data)
			continue
		}
		log.Printf("checking idle containers: %s, data = %v", containerID, data)
		// check existance
		var container *docker.Container
		container, err = pool.getInstanceByID(containerID)
		if container == nil {
			log.Printf("container already disapeared, remove from store: %s", containerID)
			pool.store.Delete(pool.makeContainerKey(containerID))
			continue
		}
		// timeout
		var lastIdleResetTimestamp, lastKeepAliveTimestamp int
		if lastKeepAliveTimestamp, err = strconv.Atoi(data[UserEventKeepAlive]); err != nil {
			log.Printf("get container keepalive time: %s, err = %v", containerID, err)
			// continue
			lastKeepAliveTimestamp = 0
		}
		if lastIdleResetTimestamp, err = strconv.Atoi(data[UserEventIdleReset]); err != nil {
			log.Printf("get container idle reset time: %s, err = %v", containerID, err)
			// continue
			lastIdleResetTimestamp = 0
		}
		var now = int(time.Now().Unix())
		var idleTimeout = now - lastIdleResetTimestamp
		if idleTimeout > pool.config.InstanceIdleTimeout {
			log.Printf("checking idle containers: %s, idle timeout: %d, kill container", containerID, idleTimeout)
			pool.KillInstance(containerID)
			continue
		}
		var keepAliveTimeout = now - lastKeepAliveTimestamp
		if keepAliveTimeout > pool.config.InstanceKeepAliveTimeout {
			log.Printf("checking idle containers: %s, keepalive timeout: %d, kill container", containerID, keepAliveTimeout)
			pool.KillInstance(containerID)
			continue
		}
	}
}

func (pool *DatalabPool) processDockerEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Printf("DONE: container docker events goroutine")
			return
		case event, ok := <-pool.dockerEvents:
			if !ok {
				log.Printf("ERROR: docker events closed, quit goroutine")
				return
			}
			if event == nil {
				break
			}
			log.Printf("docker event: %#v", event)
			switch event.Type {
			case "container":
				pool.onEventContainer(event)
			}
		}
	}
}

func (pool *DatalabPool) onEventContainer(event *docker.APIEvents) {
	switch event.Action {
	case "destroy":
		// check and cleanup user folder
		// Actor:docker.APIActor{
		//  ID:"050e5823627a2bec06a43ee5b483d150945e84f8d42500ce188f82dadc208e88",
		//  Attributes:map[string]string{
		//   "datalab_id":"1",
		//   "image":"gcr.io/cloud-datalab/datalab:latest",
		//   "name":"datalab_01"
		//  }
		// }
		var actor = event.Actor
		var containerID = actor.ID
		var err error
		var datalabID int
		if datalabID, err = strconv.Atoi(actor.Attributes["datalab_id"]); err != nil {
			log.Printf("attributes datalab_id == null, ignore container: %#v", actor.Attributes)
			break
		}
		pool.cleanupInstanceDirectory(containerID, datalabID)
	}
}

func (pool *DatalabPool) cleanupInstanceDirectory(containerID string, datalabID int) {
	os.MkdirAll(pool.config.DataBackupHome, 0755)
	var variables = pool.makeContainerVairables(datalabID)
	var datalabName = variables["datalab_name"]
	var datalabHome = variables["datalab_home"]
	var containerBackHome = filepath.Join(pool.config.DataBackupHome, datalabName+"-"+containerID)
	var err = os.Rename(datalabHome, containerBackHome)
	log.Printf("backup userdata from '%s' to '%s', err = %v", datalabHome, containerBackHome, err)
}

type HookCallback func() error

func (pool *DatalabPool) AllocateInstance(
	nonce string, filename string, content []byte,
) (containerID string, hookAfterStart HookCallback, err error) {

	pool.mutex.Lock()
	defer pool.mutex.Unlock()

	var datalabID int
	datalabID, err = pool.findNextDatalabID()
	if err != nil {
		log.Printf("could not find available datalab id")
		return
	}

	var variables map[string]string
	variables = pool.makeContainerVairables(datalabID)

	err = pool.prepareContainerDirectory(variables["datalab_home"])
	if err != nil {
		return
	}

	containerID, err = pool.createInstance(datalabID, variables)
	if err != nil {
		log.Printf("create datalab instance failure: %v, err: %v", datalabID, err)
		return
	}

	err = pool.setNonceContainer(nonce, containerID, pool.config.NonceTimeout)
	if err != nil {
		log.Printf("set nonce container failure: %v, err: %v", datalabID, err)
		return
	}

	err = pool.populateInstanceData(variables["datalab_home"], filename, content)
	if err != nil {
		log.Printf("populate instance data: %v, err = %v", datalabID, err)
		return
	}

	hookAfterStart = func() error {
		log.Printf("start running AfterStart: %v", datalabID)
		variables["default_filename"] = filename
		return pool.runScript(pool.config.HookAfterStart, variables)
	}

	return
}

func (pool *DatalabPool) populateInstanceData(datalabHome string, filename string, content []byte) (err error) {
	var (
		contentDirectory string
		fout             *os.File
		contentFilename  string
	)
	contentDirectory = filepath.Join(datalabHome, "content")
	err = os.MkdirAll(contentDirectory, 0755)
	if err != nil {
		log.Printf("create content directory failure: %s, err = %v", contentDirectory, err)
		return
	}
	contentFilename = filepath.Join(contentDirectory, filename)
	if content != nil && len(content) > 0 {
		if fout, err = os.Create(contentFilename); err != nil {
			log.Printf("create content file failure: %s, err = %v", contentFilename, err)
			return
		}
		defer fout.Close()
		fout.Write(content)
	} else {
		log.Printf("removing empty file: %v", contentFilename)
		os.Remove(contentFilename)
	}
	return
}

func (pool *DatalabPool) makeNonceKey(nonce string) string {
	return fmt.Sprintf("nonce:%s", nonce)
}

func (pool *DatalabPool) setNonceContainer(nonce string, containerID string, ttl int) (err error) {
	var key = pool.makeNonceKey(nonce)
	err = pool.store.Set(key, containerID)
	if err != nil {
		return
	}
	err = pool.store.Expire(key, ttl)
	return
}

func (pool *DatalabPool) makeContainerVairables(datalabID int) map[string]string {
	var datalabName string = fmt.Sprintf("datalab_%02d", datalabID)
	var datalabHome string = filepath.Join(pool.config.DataHome, datalabName)
	datalabHome, _ = filepath.Abs(datalabHome)
	return map[string]string{
		"datalab_id":   fmt.Sprintf("%d", datalabID),
		"datalab_name": datalabName,
		"datalab_port": fmt.Sprintf("%d", pool.config.BasePort+datalabID),
		"datalab_home": datalabHome,
	}
}

func (pool *DatalabPool) createInstance(datalabID int, variables map[string]string) (containerID string, err error) {
	// currUser, _ := user.Current()
	// ustr := fmt.Sprintf("%s:%s", currUser.Uid, currUser.Gid),
	var container *docker.Container
	var createContainerOptions = docker.CreateContainerOptions{
		Name: variables["datalab_name"],
		Config: &docker.Config{
			User:  "0",
			Image: "gcr.io/cloud-datalab/datalab:latest",
			Labels: map[string]string{
				"datalab_id": variables["datalab_id"],
			},
			Env: []string{
				`HOME=/content`,
				`DATALAB_ENV=GCE`,
				`DATALAB_DEBUG=true`,
				`DATALAB_SETTINGS_OVERRIDES={"enableAutoGCSBackups":true,"consoleLogLevel":"info"}`,
				`DATALAB_GIT_AUTHOR=backup@localhost`,
				`DATALAB_INITIAL_USER_SETTINGS=`,
			},
		},
		HostConfig: &docker.HostConfig{
			AutoRemove: pool.config.InstanceAutoRemove,
			Binds: []string{
				fmt.Sprintf("%s/content:/content", variables["datalab_home"]),
				fmt.Sprintf("%s/tmp:/tmp", variables["datalab_home"]),
			},
			PortBindings: map[docker.Port][]docker.PortBinding{
				"8080/tcp": []docker.PortBinding{
					docker.PortBinding{HostIP: "0.0.0.0", HostPort: variables["datalab_port"]},
				},
			},
		},
	}
	container, err = pool.dockerClient.CreateContainer(createContainerOptions)

	createOptions, _ := yaml.Marshal(createContainerOptions)
	log.Printf("create container options:\n%v", string(createOptions))
	containerDetails, _ := yaml.Marshal(container)
	log.Printf("created container details:\n%v", string(containerDetails))

	if err != nil {
		log.Printf("create instance failure: %v, err: %v", datalabID, err)
		return
	}

	err = pool.dockerClient.StartContainer(container.ID, nil)
	if err != nil {
		log.Printf("start instance failure: %v - %v, err: %v", container.ID, container.Name, err)
		return
	}

	pool.UpdateInstanceUserEvent(container.ID, UserEventIdleReset)
	pool.UpdateInstanceUserEvent(container.ID, UserEventKeepAlive)

	containerID = container.ID
	return
}

func (pool *DatalabPool) prepareContainerDirectory(datalabHome string) (err error) {
	var contentDirectory = filepath.Join(datalabHome, "content")
	err = os.MkdirAll(contentDirectory, 0755)
	return
}

func (pool *DatalabPool) runScript(script string, variables map[string]string) (err error) {
	log.Printf("running script: %s", strings.Replace(script, "\n", " ", -1))
	log.Printf("running script variables: %#v", variables)

	var startCommandLine string
	startCommandLine = pool.replaceStringVariables(script, variables)

	var cmdList []string
	cmdList = strings.Split(strings.TrimSpace(startCommandLine), "\n")
	if len(cmdList) == 0 {
		log.Printf("cmdline is empty: %v", cmdList)
		return
	}

	var deadLine = time.Now().Add(time.Duration(pool.config.HookScriptMaxExecutionSeconds) * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadLine)
	defer cancel()

	pool.executeCommandAndWait(ctx, cmdList)

	log.Printf("run script success: %v", cmdList)
	return
}

func (pool *DatalabPool) executeCommandAndWait(ctx context.Context, cmdList []string) {
	var cmd *exec.Cmd
	cmd = exec.CommandContext(ctx, cmdList[0], cmdList[1:]...)

	var err error
	var cmdStdoutReader, cmdStderrReader io.ReadCloser
	cmdStdoutReader, err = cmd.StdoutPipe()
	cmdStderrReader, err = cmd.StderrPipe()
	cmd.Start()

	go CtxCopy(ctx, os.Stdout, cmdStdoutReader)
	go CtxCopy(ctx, os.Stderr, cmdStderrReader)

	if err = cmd.Wait(); err != nil {
		log.Printf("run script failure: %v, err = %v", cmdList, err)
		return
	}
}

func (pool *DatalabPool) replaceStringVariables(source string, variables map[string]string) string {
	for k, v := range variables {
		var replacer string
		replacer = fmt.Sprintf("${%s}", k)
		source = strings.Replace(source, replacer, v, -1)
	}
	return source
}

func (pool *DatalabPool) findNextDatalabID() (id int, err error) {
	var containers []docker.APIContainers
	containers, err = pool.dockerClient.ListContainers(docker.ListContainersOptions{
		All: true,
		Filters: map[string][]string{
			"label": []string{"datalab_id"},
		},
	})
	if err != nil {
		return
	}
	var ports = make(map[int]string)
	for _, item := range containers {
		var port int
		var err2 error
		if port, err2 = strconv.Atoi(item.Labels["datalab_id"]); err2 != nil {
			continue
		}
		ports[port] = item.ID
	}
	for id = 1; id <= pool.config.MaxInstanceCount; id++ {
		if _, ok := ports[id]; !ok {
			return
		}
	}
	id = 0
	err = ErrDatalabInstanceNotAvailable
	return
}

func (pool *DatalabPool) GetInstanceByDatalabID(datalabID int) (containerID string, err error) {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	var containers []docker.APIContainers
	containers, err = pool.dockerClient.ListContainers(docker.ListContainersOptions{
		Filters: map[string][]string{
			"label": []string{
				fmt.Sprintf("datalab_id=%d", datalabID),
			},
		},
	})
	if err != nil {
		return
	}
	if len(containers) < 1 {
		log.Printf("containers not found: %d, count = %d", datalabID, len(containers))
		err = fmt.Errorf("container not found or not unique")
		return
	}

	containerID = containers[0].ID

	return
}

func (pool *DatalabPool) GetInstanceByNonce(nonce string, checkRunning bool) (containerID string, err error) {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	var key = pool.makeNonceKey(nonce)
	containerID, err = pool.store.Get(key)
	if err != nil {
		log.Printf("nonce not found: %v, err = %v", nonce, err)
		return
	}

	if checkRunning {
		var container *docker.Container
		container, err = pool.getInstanceByID(containerID)
		if err != nil {
			log.Printf("container id not found: %v", containerID)
			return
		}
		if !container.State.Running {
			err = ErrDatalabInstanceNotRunning
			log.Printf("container id not running: %v", containerID)
			return
		}
	}

	return
}

func (pool *DatalabPool) getInstanceByID(containerID string) (container *docker.Container, err error) {
	container, err = pool.dockerClient.InspectContainer(containerID)
	return
}

func (pool *DatalabPool) GetInstanceAddressByContainerID(containerID string) (instAddress string, err error) {
	pool.mutex.RLock()
	defer pool.mutex.RUnlock()

	var container *docker.Container
	container, err = pool.getInstanceByID(containerID)
	if err != nil {
		log.Printf("container not found: %v", containerID)
		return
	}

	if !container.State.Running {
		err = ErrDatalabInstanceNotRunning
		log.Printf("container not running: %v", containerID)
		return
	}

	var (
		idstr     string
		datalabID int
		variables map[string]string
	)
	idstr = container.Config.Labels["datalab_id"]
	datalabID, err = strconv.Atoi(idstr)
	if err != nil {
		log.Printf("label datalab_id is invalid: containerID = %s, label = %s, err = %v", containerID, idstr, err)
		return
	}

	variables = pool.makeContainerVairables(datalabID)
	instAddress = fmt.Sprintf("http://127.0.0.1:%s", variables["datalab_port"])

	return
}

func (pool *DatalabPool) KillInstance(containerID string) (err error) {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	log.Printf("start killing instance: %s", containerID)
	err = pool.dockerClient.KillContainer(docker.KillContainerOptions{
		ID: containerID,
	})
	pool.store.Delete(pool.makeContainerKey(containerID))
	return
}

const (
	UserEventIdleReset = "UserEventIdleReset"
	UserEventKeepAlive = "UserEventKeepAlive"
)

func (pool *DatalabPool) UpdateInstanceUserEvent(containerID string, event string) {
	var containerKey = pool.makeContainerKey(containerID)
	var now = fmt.Sprintf("%d", time.Now().Unix())
	pool.store.HSet(containerKey, event, now)
}

func (pool *DatalabPool) makeContainerKey(containerID string) string {
	return fmt.Sprintf("container:%s", containerID)
}
