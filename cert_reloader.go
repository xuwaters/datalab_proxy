package main

import (
	"context"
	"crypto/tls"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type CertReloader struct {
	mutex    *sync.RWMutex
	cert     *tls.Certificate
	certFile string
	keyFile  string
}

func NewCertReloader(
	ctx context.Context,
	certFile string, keyFile string,
	reloadSignals ...os.Signal,
) (reloader *CertReloader, err error) {
	reloader = &CertReloader{
		certFile: certFile,
		keyFile:  keyFile,
	}

	if err = reloader.tryReload(); err != nil {
		return
	}

	go reloader.listenReloadSignal(ctx, reloadSignals...)

	return
}

type GetCertificateFunc func(*tls.ClientHelloInfo) (*tls.Certificate, error)

func (reloader *CertReloader) CreateGetCertificateFunc() GetCertificateFunc {
	return func(*tls.ClientHelloInfo) (cert *tls.Certificate, err error) {
		reloader.mutex.RLock()
		defer reloader.mutex.RUnlock()
		cert = reloader.cert
		return
	}
}

func (reloader *CertReloader) tryReload() (err error) {
	var cert tls.Certificate
	cert, err = tls.LoadX509KeyPair(reloader.certFile, reloader.keyFile)
	if err != nil {
		return
	}
	reloader.mutex.Lock()
	defer reloader.mutex.Unlock()
	reloader.cert = &cert
	return
}

func (reloader *CertReloader) listenReloadSignal(ctx context.Context, reloadSignals ...os.Signal) {
	c := make(chan os.Signal, 1)
	if len(reloadSignals) == 0 {
		reloadSignals = []os.Signal{syscall.SIGUSR1}
	}
	signal.Notify(c, reloadSignals...)
	for {
		select {
		case <-ctx.Done():
			return
		case sig := <-c:
			log.Printf("Signal Received: '%v', try to reload TLS certificate and key from '%s' and '%s'",
				sig, reloader.certFile, reloader.keyFile)
			if err := reloader.tryReload(); err != nil {
				log.Printf("reload error, ignore: err = %v", err)
			}
		}
	}
}
