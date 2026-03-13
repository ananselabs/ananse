package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

type CertWatcher struct {
	certPath   string
	keyPath    string
	caPath     string
	tlsConfig  *tls.Config
	mu         sync.RWMutex
	watcher    *fsnotify.Watcher
	reloadChan chan struct{}
	stopCh     chan struct{}
}

func NewCertWatcher(certDir string) (*CertWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	cw := &CertWatcher{
		certPath:   filepath.Join(certDir, "tls.crt"),
		keyPath:    filepath.Join(certDir, "tls.key"),
		caPath:     filepath.Join(certDir, "ca.crt"),
		watcher:    watcher,
		reloadChan: make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
	}

	// Initial load
	if err := cw.reload(); err != nil {
		return nil, err
	}

	// Watch cert directory
	if err := watcher.Add(certDir); err != nil {
		return nil, err
	}

	Logger.Info("cert watcher initialized",
		zap.String("cert_dir", certDir))

	return cw, nil
}

func (cw *CertWatcher) Start() {
	go cw.watch()
	go cw.periodicCheck()
}

func (cw *CertWatcher) watch() {
	// Debounce timer (K8s Secret updates trigger multiple events)
	var debounceTimer *time.Timer
	debounceDelay := 500 * time.Millisecond

	for {
		select {
		case event, ok := <-cw.watcher.Events:
			if !ok {
				return
			}

			// Only care about writes to cert files
			if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
				continue
			}

			filename := filepath.Base(event.Name)
			if filename != "tls.crt" && filename != "tls.key" && filename != "ca.crt" {
				continue
			}

			Logger.Info("cert file changed",
				zap.String("file", event.Name))

			// Debounce (K8s updates all files rapidly)
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(debounceDelay, func() {
				select {
				case cw.reloadChan <- struct{}{}:
				default:
				}
			})

		case err, ok := <-cw.watcher.Errors:
			if !ok {
				return
			}
			Logger.Error("cert watcher error", zap.Error(err))

		case <-cw.reloadChan:
			if err := cw.reload(); err != nil {
				Logger.Error("failed to reload certs", zap.Error(err))
			} else {
				Logger.Info("certificates reloaded successfully")
			}

		case <-cw.stopCh:
			return
		}
	}
}

// periodicCheck checks cert expiry every hour
func (cw *CertWatcher) periodicCheck() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			cw.checkExpiry()
		case <-cw.stopCh:
			return
		}
	}
}

func (cw *CertWatcher) checkExpiry() {
	cw.mu.RLock()
	defer cw.mu.RUnlock()

	if len(cw.tlsConfig.Certificates) == 0 {
		return
	}

	cert := cw.tlsConfig.Certificates[0]
	if len(cert.Certificate) == 0 {
		return
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		Logger.Error("failed to parse cert for expiry check", zap.Error(err))
		return
	}

	timeUntilExpiry := time.Until(x509Cert.NotAfter)
	daysUntilExpiry := timeUntilExpiry.Hours() / 24

	if daysUntilExpiry < 30 {
		Logger.Warn("certificate expiring soon",
			zap.Float64("days_remaining", daysUntilExpiry),
			zap.Time("expires_at", x509Cert.NotAfter))
	}

	if daysUntilExpiry < 7 {
		Logger.Error("CRITICAL: certificate expires in less than 7 days",
			zap.Float64("days_remaining", daysUntilExpiry))
	}
}

func (cw *CertWatcher) reload() error {
	// Load cert pair
	cert, err := tls.LoadX509KeyPair(cw.certPath, cw.keyPath)
	if err != nil {
		return err
	}

	// Load CA
	caCert, err := ioutil.ReadFile(cw.caPath)
	if err != nil {
		return err
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return fmt.Errorf("failed to parse CA certificate")
	}

	// Create new TLS config
	newConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}

	// Atomic swap
	cw.mu.Lock()
	cw.tlsConfig = newConfig
	cw.mu.Unlock()

	Logger.Info("TLS config updated")
	return nil
}

// GetConfig returns current TLS config (thread-safe)
func (cw *CertWatcher) GetConfig() *tls.Config {
	cw.mu.RLock()
	defer cw.mu.RUnlock()
	return cw.tlsConfig.Clone()
}

// GetCAPool returns the CA certificate pool for verifying peer certs
func (cw *CertWatcher) GetCAPool() *x509.CertPool {
	cw.mu.RLock()
	defer cw.mu.RUnlock()
	return cw.tlsConfig.RootCAs
}

func (cw *CertWatcher) Stop() {
	close(cw.stopCh)
	cw.watcher.Close()
}
