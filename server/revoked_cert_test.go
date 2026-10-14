package server

import (
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zybuu-ai/abhed/config"
	"github.com/zybuu-ai/abhed/internal/tools"
)

// A request on a connection whose client certificate was revoked after its
// handshake is refused; another certificate's request is served.
func TestRevokedClientCertificateIsRefusedOnALiveConnection(t *testing.T) {
	bad := &x509.Certificate{SerialNumber: big.NewInt(7)}
	good := &x509.Certificate{SerialNumber: big.NewInt(8)}
	s := New(Options{Workspace: t.TempDir(), Config: config.Default(), Adapter: stubAdapter{},
		Registry:          tools.NewRegistry(tools.Read{}),
		ClientCertRevoked: func(c *x509.Certificate) bool { return c.SerialNumber.Cmp(bad.SerialNumber) == 0 }})
	for _, c := range []struct {
		cert *x509.Certificate
		want int
	}{{bad, http.StatusUnauthorized}, {good, http.StatusOK}} {
		req := httptest.NewRequest("GET", "/v1/health", nil)
		req.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{c.cert}}}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("serial %v: %d, want %d", c.cert.SerialNumber, rec.Code, c.want)
		}
	}
}
