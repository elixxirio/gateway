package cmd

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	jww "github.com/spf13/jwalterweatherman"
	"gitlab.com/elixxir/comms/mixmessages"
	crypto "gitlab.com/elixxir/crypto/authorize"
	"gitlab.com/elixxir/crypto/hash"
	"gitlab.com/elixxir/gateway/autocert"
	"gitlab.com/elixxir/gateway/storage"
	"gitlab.com/xx_network/comms/connect"
	"gitlab.com/xx_network/crypto/csprng"
	rsa2 "gitlab.com/xx_network/crypto/signature/rsa"
	"gitlab.com/xx_network/primitives/id"
	"gorm.io/gorm"
	"math/rand"
	"strings"
	"time"
)

const CertificateStateKey = "https_certificate"
const DnsTemplate = "%s.mainnet.cmix.rip"
const httpsEmail = "admins@xx.network"
const httpsCountry = "US"
const eabNotReadyErr = "EAB Credentials not yet ready, please try again"
const gwNotReadyErr = "Authorizer DNS not yet ready, please try again"

// StartHttpsServer gets a well-formed tls certificate and provides it to
// protocomms so it can start to listen for HTTPS
func (gw *Instance) StartHttpsServer() error {
	// Check states table for cert
	var parsedCert *x509.Certificate
	cert, key, err := loadHttpsCreds(gw.storage)
	if err != nil {
		return err
	}

	// Determine if we need to request a cert from the gateway.
	var shouldRequestNewCreds = false
	if cert != nil && key != nil {
		// If cert & key were stored, parse it & check validity
		parsed, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return errors.WithMessage(err, "Failed to parse new tls keypair")
		}
		parsedCert, err = x509.ParseCertificate(parsed.Certificate[0])
		if err != nil {
			return errors.WithMessage(err, "Failed to get x509 certificate from parsed")
		}

		if time.Now().Before(parsedCert.NotBefore) || time.Now().After(parsedCert.NotAfter) {
			jww.DEBUG.Printf("Loaded certificate has expired, requesting new credentials")
			shouldRequestNewCreds = true
		}
	} else {
		shouldRequestNewCreds = true
	}

	// If new credentials are needed, call out to get them
	if shouldRequestNewCreds {
		// Get tls certificate and key
		cert, key, err = gw.getHttpsCreds()
		if err != nil {
			return err
		}
		parsed, err := tls.X509KeyPair(cert, key)
		if err != nil {
			return errors.WithMessage(err, "Failed to parse new tls keypair")
		}
		parsedCert, err = x509.ParseCertificate(parsed.Certificate[0])
		if err != nil {
			return errors.WithMessage(err, "Failed to get x509 certificate from parsed")
		}
	}

	// Pass the issued cert & key to protocomms so it can start serving https
	err = gw.Comms.ProtoComms.ProvisionHttps(cert, key)
	if err != nil {
		return err
	}

	// Start thread which will sleep until replaceAt - replaceWindow
	expiry := parsedCert.NotAfter
	replaceAt := expiry.Add(-1 * gw.Params.ReplaceHttpsCertBuffer)
	gw.replaceCertificates(replaceAt)

	return gw.setGatewayTlsCertificate(cert)
}

// replaceCertificates starts a thread which will sleep until replaceAt, then
// call getHttpsCreds & re-provision protocomms with the new certificate
func (gw *Instance) replaceCertificates(replaceAt time.Time) {
	go func() {
		jww.DEBUG.Printf("Sleeping until %s to replace certificates...", replaceAt.String())
		time.Sleep(time.Until(replaceAt))
		newCert, newKey, err := gw.getHttpsCreds()
		if err != nil {
			jww.ERROR.Printf("Failed to get new https credentials: %+v", err)
		}
		err = gw.Comms.ProtoComms.ProvisionHttps(newCert, newKey)
		if err != nil {
			jww.ERROR.Printf("Failed to provision protocomms with new https credentials: %+v", err)
		}

		parsed, err := tls.X509KeyPair(newCert, newKey)
		if err != nil {
			jww.ERROR.Printf("Failed to parse new TLS keypair: %+v", err)
		}
		parsedCert, err := x509.ParseCertificate(parsed.Certificate[0])
		if err != nil {
			jww.ERROR.Printf("Failed to get x509 certificate from parsed keypair: %+v", err)
		}
		// Start thread which will sleep until the new cert needs to be replaced
		expiry := parsedCert.NotAfter
		nextReplaceAt := expiry.Add(-1 * gw.Params.ReplaceHttpsCertBuffer)
		gw.replaceCertificates(nextReplaceAt)
	}()
}

// getHttpsCreds is a helper for getting the tls certificate and key to pass
// into protocomms.  It will attempt to load from storage, or get a cert
// via zerossl if one is not found
func (gw *Instance) getHttpsCreds() ([]byte, []byte, error) {
	// Check states table for cert
	loadedCert, loadedKey, err := loadHttpsCreds(gw.storage)
	if err != nil {
		return nil, nil, err
	}
	if loadedCert != nil && loadedKey != nil {
		return loadedCert, loadedKey, nil
	}

	rng := csprng.NewSystemRNG()
	generatedKey, err := autocert.GenerateCertKey(rng)
	if err != nil {
		return nil, nil, errors.WithMessage(err, "Failed to generate key for autocert")
	}

	// Get Authorizer host
	authHost, ok := gw.Comms.GetHost(&id.Authorizer)
	if !ok {
		if gw.Params.AuthorizerAddress == "" {
			return nil, nil, errors.New("authorizer address is blank; cannot create host")
		} else {
			authHost, err = gw.Comms.AddHost(&id.Authorizer, gw.Params.AuthorizerAddress,
				[]byte(gw.NetInf.GetFullNdf().Get().Registration.TlsCertificate), connect.GetDefaultHostParams())
			if err != nil {
				return nil, nil, errors.WithMessage(err, "Failed to add authorizer host")
			}
		}
	}

	// It is possible for SendAuthorizerCertRequest to fail silently
	// If this happens, time out during the Issue call & start the process over
	credentialsReceived := false
	var issuedCert, issuedKey []byte
	for !credentialsReceived {
		// Request EAB credentials
		eabCredentialsReceived := false
		var eabCredResp *mixmessages.EABCredentialResponse
		for !eabCredentialsReceived {
			var err error
			eabCredResp, err = gw.Comms.SendEABCredentialRequest(authHost,
				&mixmessages.EABCredentialRequest{})
			if err != nil {
				if strings.Contains(err.Error(), eabNotReadyErr) {
					continue
				}
				return nil, nil, err
			}
			eabCredentialsReceived = true
		}

		// Register w/ autocert using EAB creds from authorizer
		err = gw.autoCert.Register(generatedKey, eabCredResp.KeyId, eabCredResp.Key,
			httpsEmail)
		if err != nil {
			return nil, nil, err
		}

		// Generate DNS name
		dnsName := fmt.Sprintf(DnsTemplate, base64.URLEncoding.EncodeToString(gw.Comms.GetId().Marshal()))

		// Get ACME token
		chalDomain, challenge, err := gw.autoCert.Request(dnsName)
		if err != nil {
			return nil, nil, err
		}

		jww.INFO.Printf("ADD TXT RECORD: %s\t%s\n", chalDomain, challenge)

		ts := time.Now()

		// Sign ACME token
		sig, err := crypto.SignCertRequest(rng, gw.Comms.GetPrivateKey(), challenge, ts)
		if err != nil {
			return nil, nil, err
		}

		// Authorizer code for this request is single-threaded - if another
		// gw is being processed, it will return a not ready error.
		// If this error is received, sleep for a random amount of time &
		// retry the request
		certReqComplete := false
		for !certReqComplete {
			// Send ACME token to name server
			_, err = gw.Comms.SendAuthorizerCertRequest(authHost,
				&mixmessages.AuthorizerCertRequest{
					GwID:      gw.Comms.GetId().Bytes(),
					Timestamp: ts.UnixNano(),
					ACMEToken: challenge,
					Signature: sig,
				})
			if err != nil {
				// If the authorizer gives a timeout/not ready err, sleep for 3-5 seconds & try again
				if strings.Contains(err.Error(), gwNotReadyErr) {
					sleep := 3*time.Second + time.Duration(rand.Intn(2*int(time.Second)))
					time.Sleep(sleep)
					continue
				}
				return nil, nil, err
			}
			certReqComplete = true
		}

		csrPem, csrDer, err := gw.autoCert.CreateCSR(dnsName, httpsEmail,
			httpsCountry, gw.Comms.GetId().String(), rng)
		if err != nil {
			return nil, nil, err
		}

		jww.INFO.Printf("Received CSR from autocert:\n\t%s", string(csrPem))

		// Get issued certificate and key from autoCert
		issuedCert, issuedKey, err = gw.autoCert.Issue(csrDer, gw.Params.AutocertIssueTimeout)
		if err != nil {
			if strings.Contains(err.Error(), autocert.TimedOutWaitingErr) {
				continue
			}
			return nil, nil, err
		}
		credentialsReceived = true
	}

	// Store the issued credentials in the states table
	err = storeHttpsCreds(issuedCert, issuedKey, gw.storage)
	if err != nil {
		return nil, nil, err
	}

	return issuedCert, issuedKey, nil
}

/* HTTPS helpers */

// storedHttpsCreds is an internal type, used to encapsulate the
// key and cert issued for https
type storedHttpsCreds struct {
	Key  []byte
	Cert []byte
}

// storeHttpCreds attempts to store a cert and key in the states table
func storeHttpsCreds(cert, key []byte, db *storage.Storage) error {
	internal := &storedHttpsCreds{
		Cert: cert,
		Key:  key,
	}
	marshalled, err := json.Marshal(internal)
	if err != nil {
		return err
	}

	return db.UpsertState(&storage.State{
		Key:   CertificateStateKey,
		Value: string(marshalled),
	})
}

// loadHttpsCreds attempts to load a cert and key from the states table
func loadHttpsCreds(db *storage.Storage) ([]byte, []byte, error) {
	val, err := db.GetStateValue(CertificateStateKey)
	if err != nil && !strings.Contains(err.Error(), gorm.ErrRecordNotFound.Error()) &&
		!strings.Contains(err.Error(), "Unable to locate state for key") {
		return nil, nil, err
	}
	if val == "" {
		return nil, nil, nil
	}
	loaded := &storedHttpsCreds{}
	err = json.Unmarshal([]byte(val), loaded)
	if err != nil {
		return nil, nil, err
	}
	return loaded.Cert, loaded.Key, nil
}

// Helper function which accepts the certificate used for https, signs it,
// and sets the GatewayCertificate on the Instance object to be sent when
// clients request it
func (gw *Instance) setGatewayTlsCertificate(cert []byte) error {
	h, err := hash.NewCMixHash()
	if err != nil {
		return err
	}
	h.Write(cert)
	sig, err := rsa2.Sign(csprng.NewSystemRNG(), gw.Comms.GetPrivateKey(), hash.CMixHash, h.Sum(nil), rsa2.NewDefaultOptions())
	if err != nil {
		return err
	}
	gw.gatewayCert = &mixmessages.GatewayCertificate{
		Certificate: cert,
		Signature:   sig,
	}
	return nil
}
