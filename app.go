package headscale

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/acme/autocert"
	"tailscale.com/tailcfg"
	"tailscale.com/wgengine/wgcfg"
)

// Config contains the initial Headscale configuration
type Config struct {
	ServerURL      string
	Addr           string
	PrivateKeyPath string
	DerpMap        *tailcfg.DERPMap

	DBhost string
	DBport int
	DBname string
	DBuser string
	DBpass string

	TLSLetsEncryptHostname      string
	TLSLetsEncryptCacheDir      string
	TLSLetsEncryptChallengeType string

	TLSCertPath string
	TLSKeyPath  string
}

// Headscale represents the base app of the service
type Headscale struct {
	cfg        Config
	dbString   string
	publicKey  *wgcfg.Key
	privateKey *wgcfg.PrivateKey

	pollMu         sync.Mutex
	clientsPolling map[uint64]chan []byte // this is by all means a hackity hack
}

// NewHeadscale returns the Headscale app
func NewHeadscale(cfg Config) (*Headscale, error) {
	content, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	privKey, err := wgcfg.ParsePrivateKey(string(content))
	if err != nil {
		return nil, err
	}
	pubKey := privKey.Public()
	h := Headscale{
		cfg: cfg,
		dbString: fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=disable", cfg.DBhost,
			cfg.DBport, cfg.DBname, cfg.DBuser, cfg.DBpass),
		privateKey: privKey,
		publicKey:  &pubKey,
	}
	err = h.initDB()
	if err != nil {
		return nil, err
	}
	h.clientsPolling = make(map[uint64]chan []byte)
	return &h, nil
}

// Redirect to our TLS url
func (h *Headscale) redirect(w http.ResponseWriter, req *http.Request) {
	target := h.cfg.ServerURL + req.URL.RequestURI()
	http.Redirect(w, req, target, http.StatusFound)
}

// Serve launches a GIN server with the Headscale API
func (h *Headscale) Serve() error {
	r := gin.Default()
	r.GET("/key", h.KeyHandler)
	r.GET("/register", h.RegisterWebAPI)
	r.POST("/machine/:id/map", h.PollNetMapHandler)
	r.POST("/machine/:id", h.RegistrationHandler)
	var err error
	if h.cfg.TLSLetsEncryptHostname != "" {
		if !strings.HasPrefix(h.cfg.ServerURL, "https://") {
			fmt.Println("WARNING: listening with TLS but ServerURL does not start with https://")
		}

		m := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(h.cfg.TLSLetsEncryptHostname),
			Cache:      autocert.DirCache(h.cfg.TLSLetsEncryptCacheDir),
		}
		s := &http.Server{
			Addr:      h.cfg.Addr,
			TLSConfig: m.TLSConfig(),
			Handler:   r,
		}
		if h.cfg.TLSLetsEncryptChallengeType == "TLS-ALPN-01" {
			// Configuration via autocert with TLS-ALPN-01 (https://tools.ietf.org/html/rfc8737)
			// The RFC requires that the validation is done on port 443; in other words, headscale
			// must be configured to run on port 443.
			err = s.ListenAndServeTLS("", "")
		} else if h.cfg.TLSLetsEncryptChallengeType == "HTTP-01" {
			// Configuration via autocert with HTTP-01. This requires listening on
			// port 80 for the certificate validation in addition to the headscale
			// service, which can be configured to run on any other port.
			go func() {
				log.Fatal(http.ListenAndServe(":http", m.HTTPHandler(http.HandlerFunc(h.redirect))))
			}()
		} else {
			return errors.New("Unknown value for TLSLetsEncryptChallengeType")
		}
		err = s.ListenAndServeTLS("", "")
	} else if h.cfg.TLSCertPath == "" {
		if !strings.HasPrefix(h.cfg.ServerURL, "http://") {
			fmt.Println("WARNING: listening without TLS but ServerURL does not start with http://")
		}
		err = r.Run(h.cfg.Addr)
	} else {
		if !strings.HasPrefix(h.cfg.ServerURL, "https://") {
			fmt.Println("WARNING: listening with TLS but ServerURL does not start with https://")
		}
		err = r.RunTLS(h.cfg.Addr, h.cfg.TLSCertPath, h.cfg.TLSKeyPath)
	}
	return err
}
