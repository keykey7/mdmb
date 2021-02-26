package device

import (
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"io"

	"github.com/groob/plist"
	"github.com/jessepeterson/cfgprofiles"
)

type MDMClient struct {
	Device     *Device
	MDMPayload *cfgprofiles.MDMPayload

	IdentityCertificate *x509.Certificate
	IdentityPrivateKey  *rsa.PrivateKey
}

func NewMDMClient2(device *Device, mdmPld *cfgprofiles.MDMPayload) (*MDMClient, error) {
	c := &MDMClient{Device: device, MDMPayload: mdmPld}
	err := c.loadIdentityFromKeychain(device.MDMIdentityKeychainUUID)
	return c, err
}

func (c *MDMClient) loadIdentityFromKeychain(uuid string) error {
	if uuid == "" {
		return errors.New("invalid keychain UUID")
	}
	kciID, err := LoadKeychainItem(c.Device.SystemKeychain(), uuid)
	if err != nil {
		return err
	}

	kciKey, err := LoadKeychainItem(c.Device.SystemKeychain(), kciID.IdentityKeyUUID)
	if err != nil {
		return err
	}

	kciCert, err := LoadKeychainItem(c.Device.SystemKeychain(), kciID.IdentityCertificateUUID)
	if err != nil {
		return err
	}

	c.IdentityPrivateKey = kciKey.Key
	c.IdentityCertificate = kciCert.Certificate
	return nil
}

func (c *MDMClient) loadMDMPayload(profileID string) error {
	if c.Device.MDMProfileIdentifier == "" {
		return errors.New("no MDM profile installed on device")
	}
	profile, err := c.Device.SystemProfileStore().Load(profileID)
	if err != nil {
		return err
	}
	mdmPlds := profile.MDMPayloads()
	if len(mdmPlds) != 1 {
		return errors.New("enrollment profile must contain one MDM payload")
	}
	c.MDMPayload = mdmPlds[0]
	return nil
}

func (c *MDMClient) enroll2(profileID string) error {
	if c.MDMPayload == nil {
		return errors.New("no MDM payload")
	}
	if !c.MDMPayload.SignMessage {
		return errors.New("non-SignMessage (mTLS) enrollment not supported")
	}

	err := c.authenticate()
	if err != nil {
		return err
	}

	err = c.tokenUpdate()
	if err != nil {
		return err
	}

	c.Device.MDMProfileIdentifier = profileID
	return nil
}

func NewMDMClient(device *Device) (*MDMClient, error) {
	c := &MDMClient{
		Device: device,
	}
	if device.MDMIdentityKeychainUUID != "" {
		var err error
		c.IdentityPrivateKey, c.IdentityCertificate, err = c.loadOrDeleteMDMIdentity(device.MDMIdentityKeychainUUID, false)
		if err != nil {
			return c, err
		}
	}
	if device.MDMProfileIdentifier != "" {
		profile, err := device.SystemProfileStore().Load(device.MDMProfileIdentifier)
		if err != nil {
			return c, err
		}
		mdmPlds := profile.MDMPayloads()
		if len(mdmPlds) != 1 {
			return c, errors.New("enrollment profile must contain an MDM payload")
		}
		c.MDMPayload = mdmPlds[0]
	}
	return c, nil
}

// Enroll attempts an Apple MDM enrollment using profile ep
func (c *MDMClient) Enroll(ep []byte, rand io.Reader) error {
	profile := &cfgprofiles.Profile{}

	err := plist.Unmarshal(ep, profile)
	if err != nil {
		return err
	}

	mdmPlds := profile.MDMPayloads()
	if len(mdmPlds) != 1 {
		return errors.New("enrollment profile must contain an MDM payload")
	}
	c.MDMPayload = mdmPlds[0]

	scepPlds := profile.SCEPPayloads()
	// TODO: support non-SCEP enrollment some day?
	if len(mdmPlds) != 1 {
		return errors.New("SCEP profile payload required")
	}
	scepPld := scepPlds[0]

	if !c.MDMPayload.SignMessage {
		return errors.New("non-SignMessage (mTLS) enrollment not supported")
	}

	c.IdentityPrivateKey, err = keyFromSCEPProfilePayload(scepPld, rand)
	if err != nil {
		return err
	}

	csrBytes, err := csrFromSCEPProfilePayload(scepPld, c.Device, rand, c.IdentityPrivateKey)
	if err != nil {
		return err
	}

	c.IdentityCertificate, err = scepNewPKCSReq(csrBytes, scepPld.PayloadContent.URL, scepPld.PayloadContent.Challenge)
	if err != nil {
		return err
	}

	err = c.authenticate()
	if err != nil {
		return err
	}

	err = c.tokenUpdate()
	if err != nil {
		return err
	}

	err = c.saveMDMIdentity()
	if err != nil {
		return err
	}

	c.Device.MDMProfileIdentifier = profile.PayloadIdentifier
	c.Device.SystemProfileStore().Install(ep)

	return nil
}

func (c *MDMClient) saveMDMIdentity() error {
	// delete old identity if it exists
	if c.Device.MDMIdentityKeychainUUID != "" {
		_, _, err := c.loadOrDeleteMDMIdentity(c.Device.MDMIdentityKeychainUUID, true)
		if err != nil {
			return err
		}
	}

	kciKey := NewKeychainItem(c.Device.SystemKeychain(), ClassKey)
	kciKey.Key = c.IdentityPrivateKey
	kciKey.Save()

	kciCert := NewKeychainItem(c.Device.SystemKeychain(), ClassCertificate)
	kciCert.Certificate = c.IdentityCertificate
	kciCert.Save()

	kciID := NewKeychainItem(c.Device.SystemKeychain(), ClassIdentity)
	kciID.IdentityKeyUUID = kciKey.UUID
	kciID.IdentityCertificateUUID = kciCert.UUID
	kciID.Save()

	c.Device.MDMIdentityKeychainUUID = kciID.UUID

	return nil
}

func (c *MDMClient) loadOrDeleteMDMIdentity(uuid string, delete bool) (*rsa.PrivateKey, *x509.Certificate, error) {
	kciID, err := LoadKeychainItem(c.Device.SystemKeychain(), c.Device.MDMIdentityKeychainUUID)
	if err != nil {
		return nil, nil, err
	}

	kciKey, err := LoadKeychainItem(c.Device.SystemKeychain(), kciID.IdentityKeyUUID)
	if err != nil {
		return nil, nil, err
	}

	kciCert, err := LoadKeychainItem(c.Device.SystemKeychain(), kciID.IdentityCertificateUUID)
	if err != nil {
		return nil, nil, err
	}

	if delete {
		kciCert.Delete()
		kciKey.Delete()
		kciID.Delete()
	}

	return kciKey.Key, kciCert.Certificate, nil
}

func (device *Device) MDMClient() (*MDMClient, error) {
	var err error
	if device.mdmClient == nil {
		device.mdmClient, err = NewMDMClient(device)
	}
	return device.mdmClient, err
}
