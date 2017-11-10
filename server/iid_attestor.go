package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math"
	"net/url"
	"path"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	ec2 "github.com/aws/aws-sdk-go/service/ec2"

	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/hcl"

	aia "github.com/spiffe/aws-iid-attestor/common"
	spi "github.com/spiffe/spire/proto/common/plugin"
	"github.com/spiffe/spire/proto/server/nodeattestor"
)

const (
	pluginName = "iid_attestor"

	maxSecondsBetweenDeviceAttachments int64 = 60
)

const awsCaCertPEM = `-----BEGIN CERTIFICATE-----
MIIDIjCCAougAwIBAgIJAKnL4UEDMN/FMA0GCSqGSIb3DQEBBQUAMGoxCzAJBgNV
BAYTAlVTMRMwEQYDVQQIEwpXYXNoaW5ndG9uMRAwDgYDVQQHEwdTZWF0dGxlMRgw
FgYDVQQKEw9BbWF6b24uY29tIEluYy4xGjAYBgNVBAMTEWVjMi5hbWF6b25hd3Mu
Y29tMB4XDTE0MDYwNTE0MjgwMloXDTI0MDYwNTE0MjgwMlowajELMAkGA1UEBhMC
VVMxEzARBgNVBAgTCldhc2hpbmd0b24xEDAOBgNVBAcTB1NlYXR0bGUxGDAWBgNV
BAoTD0FtYXpvbi5jb20gSW5jLjEaMBgGA1UEAxMRZWMyLmFtYXpvbmF3cy5jb20w
gZ8wDQYJKoZIhvcNAQEBBQADgY0AMIGJAoGBAIe9GN//SRK2knbjySG0ho3yqQM3
e2TDhWO8D2e8+XZqck754gFSo99AbT2RmXClambI7xsYHZFapbELC4H91ycihvrD
jbST1ZjkLQgga0NE1q43eS68ZeTDccScXQSNivSlzJZS8HJZjgqzBlXjZftjtdJL
XeE4hwvo0sD4f3j9AgMBAAGjgc8wgcwwHQYDVR0OBBYEFCXWzAgVyrbwnFncFFIs
77VBdlE4MIGcBgNVHSMEgZQwgZGAFCXWzAgVyrbwnFncFFIs77VBdlE4oW6kbDBq
MQswCQYDVQQGEwJVUzETMBEGA1UECBMKV2FzaGluZ3RvbjEQMA4GA1UEBxMHU2Vh
dHRsZTEYMBYGA1UEChMPQW1hem9uLmNvbSBJbmMuMRowGAYDVQQDExFlYzIuYW1h
em9uYXdzLmNvbYIJAKnL4UEDMN/FMAwGA1UdEwQFMAMBAf8wDQYJKoZIhvcNAQEF
BQADgYEAFYcz1OgEhQBXIwIdsgCOS8vEtiJYF+j9uO6jz7VOmJqO+pRlAbRlvY8T
C1haGgSI/A1uZUKs/Zfnph0oEI0/hu1IIJ/SKBDtN5lvmZ/IzbOPIJWirlsllQIQ
7zvWbGd9c9+Rm3p04oTvhup99la7kZqevJK0QRdD/6NpCKsqP/0=
-----END CERTIFICATE-----`

type IIDAttestorConfig struct {
	TrustDomain string `hcl:"trust_domain"`
}

type IIDAttestorPlugin struct {
	ConfigTime time.Time

	trustDomain string

	awsCaCertPublicKey *rsa.PublicKey

	mtx *sync.Mutex
}

func (p *IIDAttestorPlugin) spiffeID(awsAccountId, awsInstanceId string) *url.URL {
	spiffePath := path.Join("spire", "agent", pluginName, awsAccountId, awsInstanceId)
	id := &url.URL{
		Scheme: "spiffe",
		Host:   p.trustDomain,
		Path:   spiffePath,
	}
	return id
}

func (p *IIDAttestorPlugin) Attest(req *nodeattestor.AttestRequest) (*nodeattestor.AttestResponse, error) {

	var attestedData aia.IidAttestedData
	err := json.Unmarshal(req.AttestedData.Data, &attestedData)
	if err != nil {
		err = attestationStepError("unmarshaling the attestation data", err)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	var doc aia.InstanceIdentityDocument
	err = json.Unmarshal([]byte(attestedData.Document), &doc)
	if err != nil {
		err = attestationStepError("unmarshaling the IID", err)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	if req.AttestedBefore {
		err = attestationStepError("validating the IID", "the IID has been used and is no longer valid")
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	docHash := sha256.Sum256([]byte(attestedData.Document))
	if err != nil {
		err = attestationStepError("hashing the IID", err)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	sigBytes, err := base64.StdEncoding.DecodeString(attestedData.Signature)
	if err != nil {
		err = attestationStepError("base64 decoding the IID signature", err)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	p.mtx.Lock()
	defer p.mtx.Unlock()

	err = rsa.VerifyPKCS1v15(p.awsCaCertPublicKey, crypto.SHA256, docHash[:], sigBytes)
	if err != nil {
		err = attestationStepError("verifying the cryptographic signature", err)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	awsSession := session.Must(session.NewSession())

	ec2Client := ec2.New(awsSession, &aws.Config{
		Region: &doc.Region,
	})

	query := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{&doc.InstanceId},
	}

	result, err := ec2Client.DescribeInstances(query)
	if err != nil {
		err = attestationStepError("querying AWS via describe-instances", err)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	instance := result.Reservations[0].Instances[0]

	ifaceZeroDeviceIndex := *instance.NetworkInterfaces[0].Attachment.DeviceIndex

	if ifaceZeroDeviceIndex != 0 {
		innerErr := fmt.Errorf("DeviceIndex is %d", ifaceZeroDeviceIndex)
		err = attestationStepError("verifying the EC2 instance's NetworkInterface[0].DeviceIndex is 0", innerErr)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	ifaceZeroAttachTime := instance.NetworkInterfaces[0].Attachment.AttachTime

	rootDeviceIndex := -1
	for i, bdm := range instance.BlockDeviceMappings {
		if *bdm.DeviceName == *instance.RootDeviceName {
			rootDeviceIndex = i
			break
		}
	}

	if rootDeviceIndex == -1 {
		innerErr := fmt.Errorf("could not locate a device mapping with name '%s'", instance.RootDeviceName)
		err = attestationStepError("locating the root device block mapping", innerErr)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	rootDeviceAttachTime := instance.BlockDeviceMappings[rootDeviceIndex].Ebs.AttachTime

	attachTimeDisparitySeconds := int64(math.Abs(float64(ifaceZeroAttachTime.Unix() - rootDeviceAttachTime.Unix())))

	if attachTimeDisparitySeconds > maxSecondsBetweenDeviceAttachments {
		innerErr := fmt.Errorf("root BlockDeviceMapping and NetworkInterface[0] attach times differ by %d seconds", attachTimeDisparitySeconds)
		err = attestationStepError("checking the disparity device attach times", innerErr)
		return &nodeattestor.AttestResponse{Valid: false}, err
	}

	resp := &nodeattestor.AttestResponse{
		Valid:        true,
		BaseSPIFFEID: p.spiffeID(doc.AccountId, doc.InstanceId).String(),
	}

	return resp, nil
}

func attestationStepError(step string, cause error) error {
	return fmt.Errorf("Attempted AWS IID attestation but an error occured %s: %s", step, err)
}

func (p *IIDAttestorPlugin) Configure(req *spi.ConfigureRequest) (*spi.ConfigureResponse, error) {
	resp := &spi.ConfigureResponse{}

	// Parse HCL config payload into config struct
	config := &IIDAttestorConfig{}
	hclTree, err := hcl.Parse(req.Configuration)
	if err != nil {
		err := fmt.Errorf("Error parsing AWS IID Attestor configuration: %s", err)
		return resp, err
	}
	err = hcl.DecodeObject(&config, hclTree)
	if err != nil {
		err := fmt.Errorf("Error decoding AWS IID Attestor configuration: %v", err)
		return resp, err
	}

	block, _ := pem.Decode([]byte(awsCaCertPEM))

	awsCaCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		err := fmt.Errorf("Error reading the AWS CA Certificate in the AWS IID Attestor: %v", err)
		return resp, err
	}

	awsCaCertPublicKey, ok := awsCaCert.PublicKey.(*rsa.PublicKey)
	if !ok {
		err := fmt.Errorf("Error extracting the AWS CA Certificate's public key in the AWS IID Attestor: %v", err)
		return resp, err
	}

	p.mtx.Lock()
	defer p.mtx.Unlock()

	p.trustDomain = config.TrustDomain
	p.awsCaCertPublicKey = awsCaCertPublicKey

	return &spi.ConfigureResponse{}, nil
}

func (*IIDAttestorPlugin) GetPluginInfo(*spi.GetPluginInfoRequest) (*spi.GetPluginInfoResponse, error) {
	return &spi.GetPluginInfoResponse{}, nil
}

func New() nodeattestor.NodeAttestor {
	return &IIDAttestorPlugin{
		mtx: &sync.Mutex{},
	}
}

func main() {
	p := &IIDAttestorPlugin{
		mtx: &sync.Mutex{},
	}

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: nodeattestor.Handshake,
		Plugins: map[string]plugin.Plugin{
			"join_token": nodeattestor.NodeAttestorPlugin{NodeAttestorImpl: p},
		},
		GRPCServer: plugin.DefaultGRPCServer,
	})
}
