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

package controller

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/wso2/thunderid-operator/api/v1alpha1"
)

const (
	securityKeyServerCert  = "server.cert"
	securityKeyServerKey   = "server.key"
	securityKeySigningCert = "signing.cert"
	securityKeySigningKey  = "signing.key"
	securityKeyECDSACert   = "ecdsa-signing.cert"
	securityKeyECDSAKey    = "ecdsa-signing.key"
	securityKeyCryptoKey   = "crypto.key"
	securityKeyDirectAuth  = "direct_auth_secret"
)

// securityCertMountKeys are mounted read-only, individually by key name (via subPath), onto
// /opt/thunderid/config/certs/<key>. setup.sh only (re)generates a cert/key pair when it finds
// the file missing, so pre-supplying these from a stable Secret is what keeps them identical
// across pod restarts and across replicas.
var securityCertMountKeys = []string{
	securityKeyServerCert, securityKeyServerKey,
	securityKeySigningCert, securityKeySigningKey,
	securityKeyECDSACert, securityKeyECDSAKey,
	securityKeyCryptoKey,
}

// securityAllKeys is every key a security Secret — generated or user-supplied — must contain.
var securityAllKeys = append(append([]string{}, securityCertMountKeys...), securityKeyDirectAuth)

// reconcileSecuritySecret ensures a Secret holding ThunderID's TLS/JWT-signing/encryption key
// material exists for this instance, generating it once on first create. It is deliberately
// never updated afterwards: setup.sh inside the ThunderID container only regenerates a
// cert/key pair when the file is absent, so keeping this Secret's content stable across
// reconciles is what keeps the JWT signing key (and therefore already-issued session tokens)
// stable across pod restarts and across replicas.
func (r *ThunderIDInstanceReconciler) reconcileSecuritySecret(
	ctx context.Context, instance *appsv1alpha1.ThunderIDInstance) (string, error) {
	log := logf.FromContext(ctx)
	// A user-supplied Secret — named directly by spec.securitySecret, or discovered via the
	// security section of the Environment labeled for this instance when the field is unset — is
	// used as-is and never generated or modified by the operator, but it must carry every key our
	// volume mounts reference by name, or the pod will fail to start with an opaque "references
	// non-existent secret key" event.
	resolvedName, err := resolveSectionSecretRef(ctx, r.Client, instance, instance.Spec.SecuritySecret,
		func(e *appsv1alpha1.Environment) appsv1alpha1.EnvSection { return e.Spec.Security }, "security")
	if err != nil {
		return "", err
	}
	if resolvedName != "" {
		// Error messages name whichever source actually supplied resolvedName, so a misconfigured
		// explicit field and a misconfigured Environment point the user at the right place to fix it.
		source := fmt.Sprintf("spec.securitySecret %q", resolvedName)
		if instance.Spec.SecuritySecret == "" {
			source = fmt.Sprintf("Environment's security Secret %q", resolvedName)
		}
		provided := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{
			Name: resolvedName, Namespace: instance.Namespace,
		}, provided); err != nil {
			if errors.IsNotFound(err) {
				return "", fmt.Errorf("%s not found", source)
			}
			return "", err
		}
		var missing []string
		for _, key := range securityAllKeys {
			if len(provided.Data[key]) == 0 {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			return "", fmt.Errorf("%s is missing required key(s): %s",
				source, strings.Join(missing, ", "))
		}
		log.V(1).Info("using user-supplied security Secret as-is", "name", resolvedName)
		return resolvedName, nil
	}

	secretName := instance.Name + "-security"

	existing := &corev1.Secret{}
	err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: instance.Namespace}, existing)
	if err == nil {
		log.V(1).Info("security Secret already exists, reusing (never regenerated - see reconcileSecuritySecret's own doc comment)", "name", secretName)
		return secretName, nil
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	log.Info("generating new security Secret (self-signed certs, JWT signing keys, encryption key) - one-time, will not be regenerated on future reconciles", "name", secretName)
	serverCert, serverKey, err := generateSelfSignedCert("rsa")
	if err != nil {
		return "", fmt.Errorf("generating server TLS cert: %w", err)
	}
	signingCert, signingKey, err := generateSelfSignedCert("rsa")
	if err != nil {
		return "", fmt.Errorf("generating JWT signing cert: %w", err)
	}
	ecdsaCert, ecdsaKey, err := generateSelfSignedCert("ecdsa")
	if err != nil {
		return "", fmt.Errorf("generating ECDSA JWT signing cert: %w", err)
	}
	cryptoKey, err := randomHexSecret()
	if err != nil {
		return "", fmt.Errorf("generating encryption key: %w", err)
	}
	directAuthSecret, err := randomHexSecret()
	if err != nil {
		return "", fmt.Errorf("generating direct auth secret: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: instance.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			securityKeyServerCert:  serverCert,
			securityKeyServerKey:   serverKey,
			securityKeySigningCert: signingCert,
			securityKeySigningKey:  signingKey,
			securityKeyECDSACert:   ecdsaCert,
			securityKeyECDSAKey:    ecdsaKey,
			securityKeyCryptoKey:   cryptoKey,
			securityKeyDirectAuth:  directAuthSecret,
		},
	}
	_ = ctrl.SetControllerReference(instance, secret, r.Scheme)
	return secretName, r.Create(ctx, secret)
}

// generateSelfSignedCert mirrors the self-signed certs ThunderID's own setup.sh generates via
// openssl, including the on-disk key encoding each openssl invocation produces: PKCS8 "PRIVATE
// KEY" for RSA (openssl req -newkey rsa:...), SEC1 "EC PRIVATE KEY" for ECDSA (openssl ecparam
// -genkey). Matching these formats means ThunderID's own PEM parsing needs no changes to accept
// operator-supplied key material in place of setup.sh-generated material.
func generateSelfSignedCert(algo string) (certPEM, keyPEM []byte, err error) {
	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization:       []string{"WSO2"},
			OrganizationalUnit: []string{"ThunderID"},
			CommonName:         "localhost",
		},
		NotBefore:             time.Now(),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
	}

	var signer crypto.Signer
	var pub any
	var keyBlock *pem.Block
	if algo == "ecdsa" {
		template.NotAfter = time.Now().AddDate(10, 0, 0)
		priv, genErr := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if genErr != nil {
			return nil, nil, genErr
		}
		der, marshalErr := x509.MarshalECPrivateKey(priv)
		if marshalErr != nil {
			return nil, nil, marshalErr
		}
		signer, pub = priv, &priv.PublicKey
		keyBlock = &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}
	} else {
		template.NotAfter = time.Now().AddDate(1, 0, 0)
		priv, genErr := rsa.GenerateKey(rand.Reader, 2048)
		if genErr != nil {
			return nil, nil, genErr
		}
		der, marshalErr := x509.MarshalPKCS8PrivateKey(priv)
		if marshalErr != nil {
			return nil, nil, marshalErr
		}
		signer, pub = priv, &priv.PublicKey
		keyBlock = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, pub, signer)
	if err != nil {
		return nil, nil, err
	}

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(keyBlock)
	return certPEM, keyPEM, nil
}

// randomHexSecret matches setup.sh's `openssl rand -hex 32`, written without a trailing
// newline (setup.sh writes these with `printf '%s'`).
func randomHexSecret() ([]byte, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return []byte(hex.EncodeToString(buf)), nil
}
