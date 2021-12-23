package device

import (
	"context"
	"crypto"
	_ "crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha1"
	_ "crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/jessepeterson/cfgprofiles"
	scepclient "github.com/micromdm/scep/v2/client"
	"github.com/micromdm/scep/v2/cryptoutil/x509util"
	"github.com/micromdm/scep/v2/scep"
)

const defaultRSAKeySize = 1024

// borrowed from x509.go
func reverseBitsInAByte(in byte) byte {
	b1 := in>>4 | in<<4
	b2 := b1>>2&0x33 | b1<<2&0xcc
	b3 := b2>>1&0x55 | b2<<1&0xaa
	return b3
}

// borrowed from x509.go
func asn1BitLength(bitString []byte) int {
	bitLen := len(bitString) * 8

	for i := range bitString {
		b := bitString[len(bitString)-i-1]

		for bit := uint(0); bit < 8; bit++ {
			if (b>>bit)&1 == 1 {
				return bitLen
			}
			bitLen--
		}
	}

	return 0
}

// borrowed from x509.go
func newKeyUsageExtension(keyUsage int) (e pkix.Extension, err error) {
	e.Id = asn1.ObjectIdentifier{2, 5, 29, 15}
	e.Critical = true

	var a [2]byte
	a[0] = reverseBitsInAByte(byte(keyUsage))
	a[1] = reverseBitsInAByte(byte(keyUsage >> 8))

	l := 1
	if a[1] != 0 {
		l = 2
	}

	bitString := a[:l]
	e.Value, err = asn1.Marshal(asn1.BitString{Bytes: bitString, BitLength: asn1BitLength(bitString)})
	return e, err
}

func keyFromSCEPProfilePayload(pl *cfgprofiles.SCEPPayload, rand io.Reader) (*rsa.PrivateKey, error) {
	plc := pl.PayloadContent
	if plc.KeyType != "" && plc.KeyType != "RSA" {
		return nil, errors.New("only RSA keys supported")
	}
	keySize := defaultRSAKeySize
	if plc.KeySize > 0 {
		keySize = plc.KeySize
	}
	return rsa.GenerateKey(rand, keySize)
}

func replaceSCEPVars(device *Device, istrs []string) (ostrs []string) {
	// % /usr/libexec/mdmclient dumpSCEPVars
	r := strings.NewReplacer([]string{
		"%ComputerName%", device.ComputerName,
		"%HardwareUUID%", device.UDID,
		"%SerialNumber%", device.Serial,
		// "%HostName%", "TODO_HostName",
		// "%LocalHostName%", "TODO_LocalHostName",
		// "%MACAddress%", "TODO_MACAddress",
	}...)
	for _, istr := range istrs {
		ostrs = append(ostrs, r.Replace(istr))
	}
	return
}

func csrFromSCEPProfilePayload(pl *cfgprofiles.SCEPPayload, device *Device, rand io.Reader, privKey *rsa.PrivateKey) ([]byte, error) {
	plc := pl.PayloadContent

	tmpl := &x509util.CertificateRequest{
		ChallengePassword: plc.Challenge,
	}
	// macOS seems to default using just Digital Signature
	keyUsage := int(x509.KeyUsageDigitalSignature)
	if plc.KeyUsage != 0 {
		keyUsage = plc.KeyUsage
	}
	// this is a bitfield that appears to match Go/X509 definition
	keyUsageExtn, err := newKeyUsageExtension(keyUsage)
	if err != nil {
		return nil, err
	}
	tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, keyUsageExtn)
	for _, onvg := range plc.Subject {
		for _, onv := range onvg {
			if len(onv) < 2 {
				return nil, fmt.Errorf("invalid OID in SCEP payload: %v", onv)
			}
			values := replaceSCEPVars(device, onv[1:])
			switch onv[0] {
			case "C":
				tmpl.Subject.Country = values
			case "L":
				tmpl.Subject.Locality = values
			case "ST":
				// TODO: Are these interchangeable?
				tmpl.Subject.Province = values
			case "O":
				tmpl.Subject.Organization = values
			case "OU":
				tmpl.Subject.OrganizationalUnit = values
			case "CN":
				tmpl.Subject.CommonName = values[0]
			default:
				// TODO: arbitrary OIDs not yet supported
				return nil, fmt.Errorf("unhandled OID in SCEP payload: %v", onv)
			}
		}
	}
	// macOS seems to fill a default CN of the PayloadIdentifier if not present
	if tmpl.Subject.CommonName == "" {
		tmpl.Subject.CommonName = pl.PayloadIdentifier
	}
	// TODO: SANs
	return x509util.CreateCertificateRequest(rand, tmpl, privKey)
}

func selfSign() (*rsa.PrivateKey, *x509.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate serial number: %s", err)
	}

	timeNow := time.Now()
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "SCEP SIGNER",
		},
		NotBefore: timeNow,
		NotAfter:  timeNow.Add(time.Hour),

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(derBytes)
	return priv, cert, err
}

func scepNewPKCSReq(csrBytes []byte, url, challenge, caMessage string, fingerprint []byte) (*x509.Certificate, error) {
	logger := log.NewLogfmtLogger(os.Stderr)
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	cl, err := scepclient.New(url, logger)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()

	// HACK: mvk
	caMessage = ""

	resp, certNum, err := cl.GetCACert(ctx, caMessage)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	{
		if certNum > 1 {
			certs, err = scep.CACerts(resp)
			if err != nil {
				return nil, err
			}
		} else {
			certs, err = x509.ParseCertificates(resp)
			if err != nil {
				return nil, err
			}
		}
	}

	selector := scep.NopCertsSelector()
	hashType := crypto.Hash(0)
	switch len(fingerprint) {
	case 16:
		hashType = crypto.MD5
	case 20:
		hashType = crypto.SHA1
	case 32:
		hashType = crypto.SHA256
		// case 64:
		//	hashType = crypto.SHA512
	}
	if hashType != 0 {
		selector = scep.FingerprintCertsSelector(hashType, fingerprint)
	} else {
		fmt.Printf("CAFingerprint length %d not supported\n", len(fingerprint))
	}

	scepTmpKey, scepTmpCert, err := selfSign()
	if err != nil {
		return nil, err
	}

	tmpl := &scep.PKIMessage{
		MessageType: scep.PKCSReq,
		Recipients:  certs,
		SignerKey:   scepTmpKey,
		SignerCert:  scepTmpCert,
	}

	if challenge != "" {
		tmpl.CSRReqMessage = &scep.CSRReqMessage{
			ChallengePassword: challenge,
		}
	}

	csr, err := x509.ParseCertificateRequest(csrBytes)
	if err != nil {
		return nil, err
	}

	msg, err := scep.NewCSRRequest(csr, tmpl, scep.WithLogger(logger), scep.WithCertsSelector(selector))
	if err != nil {
		return nil, fmt.Errorf("creating csr pkiMessage: %w", err)
	}

	respBytes, err := cl.PKIOperation(ctx, msg.Raw)
	if err != nil {
		return nil, fmt.Errorf("PKIOperation for PKCSReq: %w", err)
	}

	respMsg, err := scep.ParsePKIMessage(respBytes, scep.WithLogger(logger), scep.WithCACerts(msg.Recipients))
	if err != nil {
		return nil, fmt.Errorf("PKCSReq parsing pkiMessage response: %w", err)
	}

	if respMsg.PKIStatus != scep.SUCCESS {
		return nil, fmt.Errorf("PKCSReq request failed: %+v", respMsg)
	}

	logger.Log("pkiStatus", "SUCCESS", "msg", "server returned a certificate.")

	if err := respMsg.DecryptPKIEnvelope(scepTmpCert, scepTmpKey); err != nil {
		return nil, fmt.Errorf("PKCSReq decrypt pkiEnvelope: %s: %w", respMsg.PKIStatus, err)
	}

	return respMsg.CertRepMessage.Certificate, nil
}
