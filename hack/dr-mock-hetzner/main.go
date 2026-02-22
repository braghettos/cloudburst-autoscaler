// dr-mock-hetzner is a mock Hetzner API server for disaster recovery testing.
// Deploy in-cluster and add hostAliases to the controller so api.hetzner.cloud
// resolves to this mock. Proxies most requests to the real API.
// POST /v1/servers returns 503 to simulate outage (controller falls back to next provider).
// DELETE /v1/servers/{id} returns 503 then 404 (cleanup after manual instance delete).
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	defaultListen = ":8089"
	realAPI      = "https://api.hetzner.cloud"
	// deleteErrorsBeforeSuccess is how many 503s to return before 404 on DELETE
	deleteErrorsBeforeSuccess = 3
)

func main() {
	listen := os.Getenv("DR_MOCK_LISTEN")
	if listen == "" {
		listen = defaultListen
	}

	real, _ := url.Parse(realAPI)
	proxy := httputil.NewSingleHostReverseProxy(real)
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = real.Scheme
		req.URL.Host = real.Host
		req.Host = real.Host
		if token := os.Getenv("HETZNER_API_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}

	var deleteCount atomic.Int32

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// POST /v1/servers -> always 503 to simulate Hetzner API outage during create
		// (controller will fall back to next recommendation, e.g. Scaleway)
		if r.Method == http.MethodPost && r.URL.Path == "/v1/servers" {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"message":"service temporarily unavailable","code":"service_unavailable"}}`))
			return
		}

		// DELETE /v1/servers/{id} -> mock 503 then 404 (for cleanup after manual instance delete)
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/servers/") {
			n := deleteCount.Add(1)
			if n <= deleteErrorsBeforeSuccess {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":{"message":"service temporarily unavailable","code":"service_unavailable"}}`))
				return
			}
			// Simulate instance already deleted
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"server not found","code":"not_found"}}`))
			return
		}

		// All other requests: proxy to real API
		proxy.ServeHTTP(w, r)
	})

	// Serve HTTPS when TLS_PORT is set (for in-cluster mock with hostAliases)
	if tlsPort := os.Getenv("TLS_PORT"); tlsPort != "" {
		cert, err := generateSelfSignedCert()
		if err != nil {
			panic(err)
		}
		server := &http.Server{
			Addr:      ":" + tlsPort,
			Handler:   handler,
			TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
		}
		panic(server.ListenAndServeTLS("", ""))
	}
	http.ListenAndServe(listen, handler)
}

func generateSelfSignedCert() (tls.Certificate, error) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "api.hetzner.cloud"},
		NotBefore:   time.Now(),
		NotAfter:    time.Now().Add(24 * time.Hour),
		DNSNames:    []string{"api.hetzner.cloud"},
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	keyBytes, _ := x509.MarshalECPrivateKey(key)
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}),
	)
}
