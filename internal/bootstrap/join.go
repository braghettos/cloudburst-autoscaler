/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	// DefaultKindControlPlaneContainer is the default kind control plane container name
	DefaultKindControlPlaneContainer = "kind-control-plane"
	// DefaultTokenTTLMinutes is the default token TTL in minutes
	DefaultTokenTTLMinutes = 30
	// CommandTimeout is the timeout for docker exec commands
	CommandTimeout = 30 * time.Second
)

// JoinCommandProvider provides kubeadm join commands
type JoinCommandProvider struct {
	containerName string
	tokenTTL      int32
	clientset     kubernetes.Interface
	restConfig    *rest.Config
}

// NewJoinCommandProvider creates a new join command provider
// If containerName is empty, it will be constructed from kindClusterName as <kindClusterName>-control-plane
// If clientset and restConfig are provided, it will use the Kubernetes API to execute commands
// Otherwise, it will fall back to kubectl exec (for backward compatibility)
func NewJoinCommandProvider(containerName, kindClusterName string, tokenTTL int32) *JoinCommandProvider {
	if containerName == "" {
		if kindClusterName != "" {
			containerName = fmt.Sprintf("%s-control-plane", kindClusterName)
		} else {
			containerName = DefaultKindControlPlaneContainer
		}
	}
	if tokenTTL == 0 {
		tokenTTL = DefaultTokenTTLMinutes
	}

	return &JoinCommandProvider{
		containerName: containerName,
		tokenTTL:      tokenTTL,
	}
}

// SetKubernetesClient sets the Kubernetes clientset and config for API-based command execution
func (j *JoinCommandProvider) SetKubernetesClient(clientset kubernetes.Interface, restConfig *rest.Config) {
	j.clientset = clientset
	j.restConfig = restConfig
}

// GetJoinCommand creates a bootstrap token and constructs the kubeadm join command
func (j *JoinCommandProvider) GetJoinCommand(ctx context.Context) (string, error) {
	// Extract kind cluster name from container name (e.g., "cloudburst-control-plane" -> "cloudburst")
	kindClusterName := strings.TrimSuffix(j.containerName, "-control-plane")
	if kindClusterName == j.containerName {
		// Fallback: try default kind cluster name
		kindClusterName = "cloudburst"
	}
	
	// Use Kubernetes API if clientset is available to create bootstrap token securely
	if j.clientset != nil && j.restConfig != nil {
		return j.getJoinCommandViaAPI(ctx, kindClusterName)
	}
	
	// Fallback to kubectl exec (for backward compatibility or when running outside cluster)
	return j.getJoinCommandViaKubectl(ctx, "", "")
}

// getJoinCommandViaAPI creates a bootstrap token Secret via Kubernetes API and constructs the join command
// This is the secure, standard way to create bootstrap tokens without requiring host access
func (j *JoinCommandProvider) getJoinCommandViaAPI(ctx context.Context, kindClusterName string) (string, error) {
	// Generate a random bootstrap token
	tokenID, err := generateTokenID()
	if err != nil {
		return "", fmt.Errorf("failed to generate token ID: %w", err)
	}
	
	tokenSecret, err := generateTokenSecret()
	if err != nil {
		return "", fmt.Errorf("failed to generate token secret: %w", err)
	}
	
	token := fmt.Sprintf("%s.%s", tokenID, tokenSecret)
	
	// Calculate expiration time
	ttl := time.Duration(j.tokenTTL) * time.Minute
	expiration := time.Now().Add(ttl).Format(time.RFC3339)
	
	// Create the bootstrap token Secret
	// Bootstrap token secrets follow the naming convention: bootstrap-token-<token-id>
	secretName := fmt.Sprintf("bootstrap-token-%s", tokenID)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: metav1.NamespaceSystem,
		},
		Type: "bootstrap.kubernetes.io/token",
		Data: map[string][]byte{
			"token-id":                       []byte(tokenID),
			"token-secret":                   []byte(tokenSecret),
			"expiration":                     []byte(expiration),
			"usage-bootstrap-signing":        []byte("true"),
			"usage-bootstrap-authentication": []byte("true"),
			"auth-extra-groups":              []byte("system:bootstrappers:kubeadm:default-node-token"),
			"description":                    []byte("Token created by cloudburst-autoscaler"),
		},
	}
	
	// Create the Secret
	_, err = j.clientset.CoreV1().Secrets(metav1.NamespaceSystem).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create bootstrap token secret: %w", err)
	}
	
	// Get the API server endpoint and CA certificate hash
	apiServerEndpoint, err := j.getAPIServerEndpoint(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get API server endpoint: %w", err)
	}
	
	caHash, err := j.getCAHash(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get CA certificate hash: %w", err)
	}
	
	// Construct the kubeadm join command
	joinCommand := fmt.Sprintf("kubeadm join %s --token %s --discovery-token-ca-cert-hash sha256:%s",
		apiServerEndpoint, token, caHash)
	
	return joinCommand, nil
}

// generateTokenID generates a random 6-character token ID (alphanumeric lowercase)
func generateTokenID() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		randomByte := make([]byte, 1)
		if _, err := rand.Read(randomByte); err != nil {
			return "", err
		}
		b[i] = charset[int(randomByte[0])%len(charset)]
	}
	return string(b), nil
}

// generateTokenSecret generates a random 16-character token secret (alphanumeric lowercase)
func generateTokenSecret() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	for i := range b {
		randomByte := make([]byte, 1)
		if _, err := rand.Read(randomByte); err != nil {
			return "", err
		}
		b[i] = charset[int(randomByte[0])%len(charset)]
	}
	return string(b), nil
}

// getAPIServerEndpoint gets the API server endpoint from the cluster config
func (j *JoinCommandProvider) getAPIServerEndpoint(ctx context.Context) (string, error) {
	// Use the REST config's host, which should be the API server endpoint
	if j.restConfig != nil && j.restConfig.Host != "" {
		// Remove https:// prefix if present
		endpoint := strings.TrimPrefix(j.restConfig.Host, "https://")
		endpoint = strings.TrimPrefix(endpoint, "http://")
		return endpoint, nil
	}
	return "", fmt.Errorf("API server endpoint not available in REST config")
}

// getCAHash gets the CA certificate hash from the cluster
func (j *JoinCommandProvider) getCAHash(ctx context.Context) (string, error) {
	// Get the CA certificate from the config map
	cm, err := j.clientset.CoreV1().ConfigMaps(metav1.NamespaceSystem).Get(ctx, "kube-root-ca.crt", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get kube-root-ca.crt configmap: %w", err)
	}
	
	caCert, ok := cm.Data["ca.crt"]
	if !ok {
		return "", fmt.Errorf("ca.crt not found in kube-root-ca.crt configmap")
	}
	
	// Calculate SHA256 hash of the CA certificate
	// The hash is the first 64 characters of the SHA256 hash in hex format
	hasher := sha256.New()
	hasher.Write([]byte(caCert))
	hash := hex.EncodeToString(hasher.Sum(nil))
	return hash, nil
}

// getJoinCommandViaKubectl uses kubectl exec as a fallback
func (j *JoinCommandProvider) getJoinCommandViaKubectl(ctx context.Context, podName, namespace string) (string, error) {
	// This would require importing os/exec and implementing kubectl exec
	// For now, return an error suggesting to use the API method
	return "", fmt.Errorf("kubectl exec fallback not implemented; use SetKubernetesClient to enable API-based execution")
}
