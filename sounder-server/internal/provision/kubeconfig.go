package provision

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// kubeconfigFile 只解析 provisioner 需要的字段（server + CA + 认证）。
type kubeconfigFile struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string `yaml:"token"`
			TokenFile             string `yaml:"tokenFile"`
			ClientCertificateData string `yaml:"client-certificate-data"`
			ClientKeyData         string `yaml:"client-key-data"`
			ClientCertificate     string `yaml:"client-certificate"`
			ClientKey             string `yaml:"client-key"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

type kubeAuth struct {
	server  string
	token   string
	tlsConf *tls.Config
}

// loadKubeconfig 读取 KUBECONFIG 或 ~/.kube/config，供集群外调试使用。
func loadKubeconfig() (*kubeAuth, error) {
	path := os.Getenv("KUBECONFIG")
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".kube", "config")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	var cfg kubeconfigFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}

	ctxIdx := 0
	if cfg.CurrentContext != "" {
		for i, c := range cfg.Contexts {
			if c.Name == cfg.CurrentContext {
				ctxIdx = i
				break
			}
		}
	}
	if len(cfg.Contexts) == 0 {
		return nil, fmt.Errorf("kubeconfig 没有 context")
	}
	ctx := cfg.Contexts[ctxIdx].Context

	var cluster struct {
		Server                   string
		CertificateAuthorityData string
		CertificateAuthority     string
		InsecureSkipTLSVerify    bool
	}
	for _, c := range cfg.Clusters {
		if c.Name == ctx.Cluster {
			cluster.Server = c.Cluster.Server
			cluster.CertificateAuthorityData = c.Cluster.CertificateAuthorityData
			cluster.CertificateAuthority = c.Cluster.CertificateAuthority
			cluster.InsecureSkipTLSVerify = c.Cluster.InsecureSkipTLSVerify
			break
		}
	}
	if cluster.Server == "" {
		return nil, fmt.Errorf("kubeconfig 缺少 cluster.server")
	}

	auth := &kubeAuth{server: strings.TrimRight(cluster.Server, "/")}
	auth.tlsConf = &tls.Config{MinVersion: tls.VersionTLS12}

	// CA
	if cluster.CertificateAuthorityData != "" {
		if ca, err := base64.StdEncoding.DecodeString(cluster.CertificateAuthorityData); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(ca) {
				auth.tlsConf.RootCAs = pool
			}
		}
	} else if cluster.CertificateAuthority != "" {
		if ca, err := os.ReadFile(cluster.CertificateAuthority); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(ca) {
				auth.tlsConf.RootCAs = pool
			}
		}
	} else if cluster.InsecureSkipTLSVerify {
		auth.tlsConf.InsecureSkipVerify = true
	}

	// 认证：token 或客户端证书
	for _, u := range cfg.Users {
		if u.Name != ctx.User {
			continue
		}
		if u.User.Token != "" {
			auth.token = u.User.Token
		} else if u.User.TokenFile != "" {
			if b, err := os.ReadFile(u.User.TokenFile); err == nil {
				auth.token = strings.TrimSpace(string(b))
			}
		}
		certPEM := decodeMaybeBase64(u.User.ClientCertificateData)
		keyPEM := decodeMaybeBase64(u.User.ClientKeyData)
		if certPEM == nil && u.User.ClientCertificate != "" {
			certPEM, _ = os.ReadFile(u.User.ClientCertificate)
		}
		if keyPEM == nil && u.User.ClientKey != "" {
			keyPEM, _ = os.ReadFile(u.User.ClientKey)
		}
		if len(certPEM) > 0 && len(keyPEM) > 0 {
			if cert, err := tls.X509KeyPair(certPEM, keyPEM); err == nil {
				auth.tlsConf.Certificates = []tls.Certificate{cert}
			}
		}
		break
	}
	return auth, nil
}

func decodeMaybeBase64(s string) []byte {
	if s == "" {
		return nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	// 已经是 PEM 文本
	if block, _ := pem.Decode([]byte(s)); block != nil {
		return []byte(s)
	}
	return nil
}
