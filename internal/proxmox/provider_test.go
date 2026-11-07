package proxmox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liorokman/proxmox-cloud-provider/internal/certtest"
	cloudprovider "k8s.io/cloud-provider"
)

const sampleConfig = `
security:
  cert: /etc/lb/cert.pem
  key: /etc/lb/key.pem
  ca: /etc/lb/ca.pem
managerHost: 192.168.67.4
managerPort: 9999
proxmoxConfig:
  url: https://pve.example:8006/api2/json
  timeout: 7
  apiToken: /etc/pve/token
  username: /etc/pve/user
  insecureTLS: true
  caCert: /etc/pve/ca.pem
  debug: true
`

func TestNewParsesTheConfiguration(t *testing.T) {
	cp, err := New(strings.NewReader(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	p := cp.(*Provider)
	c := p.config
	if c.Security.CertFile != "/etc/lb/cert.pem" || c.Security.KeyFile != "/etc/lb/key.pem" || c.Security.CAFile != "/etc/lb/ca.pem" ||
		c.ManagerHost != "192.168.67.4" || c.ManagerPort != 9999 {
		t.Errorf("unexpected config %+v", c)
	}
	cc := c.ClusterConfig
	if cc.URL != "https://pve.example:8006/api2/json" || cc.Timeout != 7 || cc.APIToken != "/etc/pve/token" || cc.Username != "/etc/pve/user" ||
		!cc.InsecureTLS || cc.CACert != "/etc/pve/ca.pem" || !cc.Debug {
		t.Errorf("unexpected cluster config %+v", cc)
	}
}

func TestNewRejectsBadInput(t *testing.T) {
	if _, err := New(strings.NewReader("managerPort: [not, a, number]")); err == nil {
		t.Error("invalid YAML types must be refused")
	}
	if _, err := New(errReader{}); err == nil {
		t.Error("a read error must be reported")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, os.ErrClosed }

func TestProviderCapabilities(t *testing.T) {
	cp, err := New(strings.NewReader(sampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	p := cp.(*Provider)
	if p.ProviderName() != "proxmox" || !p.HasClusterID() {
		t.Error("name or cluster id")
	}
	if _, ok := p.Instances(); ok {
		t.Error("the deprecated Instances interface is not supported")
	}
	if _, ok := p.Zones(); ok {
		t.Error("the deprecated Zones interface is not supported")
	}
	if _, ok := p.Clusters(); ok {
		t.Error("Clusters is not supported")
	}
	if _, ok := p.Routes(); ok {
		t.Error("Routes is not supported")
	}
	if _, ok := p.InstancesV2(); !ok {
		t.Error("InstancesV2 is supported")
	}
	if _, ok := p.LoadBalancer(); !ok {
		t.Error("LoadBalancer is supported")
	}
	var _ cloudprovider.Interface = p
}

func TestLoadKeypair(t *testing.T) {
	dir := t.TempDir()
	f := certtest.Write(t, dir)

	if creds, err := loadKeypair(Security{CertFile: f.Cert, KeyFile: f.Key, CAFile: f.CA}); err != nil || creds == nil {
		t.Fatalf("valid files: %v %v", creds, err)
	}
	// The key may be in the certificate file
	combined := filepath.Join(dir, "both.pem")
	cert, _ := os.ReadFile(f.Cert)
	key, _ := os.ReadFile(f.Key)
	if err := os.WriteFile(combined, append(cert, key...), 0o600); err != nil {
		t.Fatal(err)
	}
	if creds, err := loadKeypair(Security{CertFile: combined, CAFile: f.CA}); err != nil || creds == nil {
		t.Errorf("combined file: %v %v", creds, err)
	}

	if _, err := loadKeypair(Security{CertFile: filepath.Join(dir, "missing.pem"), KeyFile: f.Key, CAFile: f.CA}); err == nil {
		t.Error("a missing certificate must be an error")
	}
	if _, err := loadKeypair(Security{CertFile: f.Cert, KeyFile: f.Key, CAFile: filepath.Join(dir, "missing.pem")}); err == nil {
		t.Error("a missing CA must be an error")
	}
	// A CA file without a certificate in it must be an error, not an empty credential
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := loadKeypair(Security{CertFile: f.Cert, KeyFile: f.Key, CAFile: junk})
	if err == nil || creds != nil {
		t.Errorf("an unusable CA must be an error: %v %v", creds, err)
	}
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewInstances(t *testing.T) {
	pve := newFakePVE(t)
	dir := t.TempDir()
	user := writeFile(t, dir, "user", "test@pve!t\n")
	token := writeFile(t, dir, "token", "secret\n")
	certs := certtest.Write(t, dir)

	good := ClusterConfig{URL: pve.url(), Username: user, APIToken: token, Timeout: 5}
	if i, err := newInstances(good); err != nil || i == nil {
		t.Fatalf("good config: %v %v", i, err)
	}
	withCA := good
	withCA.CACert = certs.CA
	if _, err := newInstances(withCA); err != nil {
		t.Errorf("with a CA: %v", err)
	}
	insecure := good
	insecure.InsecureTLS, insecure.Debug = true, true
	if _, err := newInstances(insecure); err != nil {
		t.Errorf("insecure: %v", err)
	}

	missingUser := good
	missingUser.Username = filepath.Join(dir, "none")
	if _, err := newInstances(missingUser); err == nil {
		t.Error("a missing user file must be an error")
	}
	missingToken := good
	missingToken.APIToken = filepath.Join(dir, "none")
	if _, err := newInstances(missingToken); err == nil {
		t.Error("a missing token file must be an error")
	}
	missingCA := good
	missingCA.CACert = filepath.Join(dir, "none")
	if _, err := newInstances(missingCA); err == nil {
		t.Error("a missing CA file must be an error")
	}
	badCA := good
	badCA.CACert = writeFile(t, dir, "junk", "not a certificate")
	if _, err := newInstances(badCA); err == nil {
		t.Error("an unusable CA must be an error")
	}

	pve.set("/version", 401)
	if _, err := newInstances(good); err == nil {
		t.Error("an API that refuses the token must be an error")
	}
}

func TestInitialize(t *testing.T) {
	pve := newFakePVE(t)
	dir := t.TempDir()
	certs := certtest.Write(t, dir)
	cfg := ProviderConfig{
		Security:    Security{CertFile: certs.Cert, KeyFile: certs.Key, CAFile: certs.CA},
		ManagerHost: "127.0.0.1",
		ManagerPort: 9,
		ClusterConfig: ClusterConfig{
			URL:      pve.url(),
			Username: writeFile(t, dir, "user", "test@pve!t"),
			APIToken: writeFile(t, dir, "token", "secret"),
		},
	}
	p := &Provider{config: cfg}
	stop := make(chan struct{})
	p.Initialize(nil, stop)
	close(stop)

	if p.instances == nil || p.lbManager == nil || p.lbManager.client == nil {
		t.Fatalf("not initialized: %+v", p)
	}
	if lb, ok := p.LoadBalancer(); !ok || lb == nil {
		t.Error("the load balancer interface is missing")
	}
	if i, ok := p.InstancesV2(); !ok || i == nil {
		t.Error("the instances interface is missing")
	}
}
