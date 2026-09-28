package ateclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/rashadism/ate-oc/third_party/ateapipb"
)

const serverName = "api.ate-system.svc"

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T) testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return testCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a PEM bundle with the private key followed by the leaf certificate.
func (ca testCA) issue(t *testing.T, dns string, uri string) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	if dns != "" {
		tmpl.DNSNames = []string{dns}
	}
	if uri != "" {
		u, _ := url.Parse(uri)
		tmpl.URIs = []*url.URL{u}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	return append(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
}

type identityServer struct {
	ateapipb.UnimplementedControlServer
}

func (identityServer) GetAtespace(ctx context.Context, _ *ateapipb.GetAtespaceRequest) (*ateapipb.Atespace, error) {
	p, _ := peer.FromContext(ctx)
	id := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0].URIs[0].String()
	return &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: id}}, nil
}

// startServer serves over mTLS; the serving certificate is swappable.
func startServer(t *testing.T, clientCA testCA, serving *atomic.Pointer[tls.Certificate]) string {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	creds := credentials.NewTLS(&tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return serving.Load(), nil
		},
	})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer(grpc.Creds(creds))
	ateapipb.RegisterControlServer(s, identityServer{})
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func mustKeyPair(t *testing.T, bundle []byte) *tls.Certificate {
	t.Helper()
	c, err := tls.X509KeyPair(bundle, bundle)
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func whoami(t *testing.T, cfg Config) (string, error) {
	t.Helper()
	c, err := Dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	as, err := c.GetAtespace(ctx, &ateapipb.GetAtespaceRequest{})
	if err != nil {
		return "", err
	}
	return as.GetMetadata().GetName(), nil
}

func TestDialMTLS(t *testing.T) {
	dir := t.TempDir()
	serverCA, clientCA := newCA(t), newCA(t)

	var serving atomic.Pointer[tls.Certificate]
	serving.Store(mustKeyPair(t, serverCA.issue(t, serverName, "")))
	addr := startServer(t, clientCA, &serving)

	cfg := Config{
		Target:           "passthrough:///" + addr,
		ServerName:       serverName,
		CredentialBundle: filepath.Join(dir, "credential-bundle.pem"),
		CAFile:           filepath.Join(dir, "trust-bundle.pem"),
	}
	write(t, cfg.CredentialBundle, clientCA.issue(t, "", "spiffe://cluster.local/ns/oc/sa/one"))
	write(t, cfg.CAFile, serverCA.pem)

	t.Run("presents client identity", func(t *testing.T) {
		id, err := whoami(t, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if id != "spiffe://cluster.local/ns/oc/sa/one" {
			t.Fatalf("server saw %q", id)
		}
	})

	t.Run("reloads rotated client certificate", func(t *testing.T) {
		write(t, cfg.CredentialBundle, clientCA.issue(t, "", "spiffe://cluster.local/ns/oc/sa/two"))
		id, err := whoami(t, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if id != "spiffe://cluster.local/ns/oc/sa/two" {
			t.Fatalf("server saw %q", id)
		}
	})

	t.Run("rejects wrong server name", func(t *testing.T) {
		bad := cfg
		bad.ServerName = "other.ate-system.svc"
		if _, err := whoami(t, bad); err == nil {
			t.Fatal("expected handshake failure")
		}
	})

	t.Run("reloads rotated CA", func(t *testing.T) {
		newServerCA := newCA(t)
		serving.Store(mustKeyPair(t, newServerCA.issue(t, serverName, "")))
		if _, err := whoami(t, cfg); err == nil {
			t.Fatal("expected failure before the CA file is rotated")
		}
		write(t, cfg.CAFile, append(serverCA.pem, newServerCA.pem...))
		if _, err := whoami(t, cfg); err != nil {
			t.Fatal(err)
		}
	})
}

func TestTLSConfigRequiresFiles(t *testing.T) {
	if _, err := TLSConfig(Config{ServerName: serverName, CredentialBundle: "/nope", CAFile: "/nope"}); err == nil {
		t.Fatal("expected error for missing files")
	}
	if _, err := TLSConfig(Config{}); err == nil {
		t.Fatal("expected error for empty config")
	}
}
