package ateclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/rashadism/ate-oc/third_party/ateapipb"
)

const roundRobinServiceConfig = `{"loadBalancingConfig": [{"round_robin":{}}]}`

type Config struct {
	// Target is the gRPC target, e.g. dns:///api.ate-system.svc:443.
	Target string
	// ServerName is the DNS name the ateapi serving certificate must carry.
	ServerName string
	// CredentialBundle is a PEM file holding the client key and certificate chain.
	CredentialBundle string
	// CAFile is a PEM file with the CAs that sign the ateapi serving certificate.
	CAFile string
}

type Client struct {
	ateapipb.ControlClient
	conn *grpc.ClientConn
}

func (c *Client) Close() error { return c.conn.Close() }

func Dial(cfg Config, opts ...grpc.DialOption) (*Client, error) {
	tlsCfg, err := TLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	opts = append([]grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithDefaultServiceConfig(roundRobinServiceConfig),
		grpc.WithChainUnaryInterceptor(RetryAborted(defaultRetry)),
	}, opts...)
	conn, err := grpc.NewClient(cfg.Target, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial ateapi: %w", err)
	}
	return &Client{ControlClient: ateapipb.NewControlClient(conn), conn: conn}, nil
}

// TLSConfig re-reads the credential bundle and CA file whenever they change on
// disk, so rotated pod certificates and trust bundles apply to new handshakes.
func TLSConfig(cfg Config) (*tls.Config, error) {
	if cfg.ServerName == "" || cfg.CredentialBundle == "" || cfg.CAFile == "" {
		return nil, errors.New("ateclient: ServerName, CredentialBundle and CAFile are required")
	}
	certs := &fileCache[*tls.Certificate]{path: cfg.CredentialBundle, parse: parseBundle}
	roots := &fileCache[*x509.CertPool]{path: cfg.CAFile, parse: parsePool}
	if _, err := certs.get(); err != nil {
		return nil, err
	}
	if _, err := roots.get(); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return certs.get()
		},
		// Chain verification happens in VerifyConnection against the reloadable pool.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(cs tls.ConnectionState) error {
			pool, err := roots.get()
			if err != nil {
				return err
			}
			if len(cs.PeerCertificates) == 0 {
				return errors.New("ateclient: server presented no certificate")
			}
			inter := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				inter.AddCert(c)
			}
			_, err = cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         pool,
				Intermediates: inter,
				DNSName:       cfg.ServerName,
			})
			return err
		},
	}, nil
}

func parseBundle(b []byte) (*tls.Certificate, error) {
	c, err := tls.X509KeyPair(b, b)
	if err != nil {
		return nil, fmt.Errorf("parse credential bundle: %w", err)
	}
	return &c, nil
}

func parsePool(b []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("no certificates in CA file")
	}
	return pool, nil
}

type fileCache[T any] struct {
	path  string
	parse func([]byte) (T, error)

	mu      sync.Mutex
	modTime time.Time
	size    int64
	val     T
	loaded  bool
}

func (c *fileCache[T]) get() (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, err := os.Stat(c.path)
	if err != nil {
		if c.loaded {
			return c.val, nil
		}
		return c.val, fmt.Errorf("ateclient: %w", err)
	}
	if c.loaded && st.ModTime().Equal(c.modTime) && st.Size() == c.size {
		return c.val, nil
	}
	b, err := os.ReadFile(c.path)
	if err != nil {
		return c.val, fmt.Errorf("ateclient: %w", err)
	}
	v, err := c.parse(b)
	if err != nil {
		if c.loaded {
			return c.val, nil
		}
		return c.val, fmt.Errorf("ateclient: %s: %w", c.path, err)
	}
	c.val, c.modTime, c.size, c.loaded = v, st.ModTime(), st.Size(), true
	return v, nil
}
