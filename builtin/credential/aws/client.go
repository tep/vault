package awsauth

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/aws/aws-sdk-go/service/sts/stsiface"
	"github.com/hashicorp/errwrap"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/hashicorp/vault/sdk/helper/awsutil"
	"github.com/hashicorp/vault/sdk/logical"
)

var (
	// These variables are intended to be set by tests. If set, the given
	// client will override the AWS client, allowing client responses to
	// be mocked out.
	mockEC2Client ec2iface.EC2API = nil
	mockIAMClient iamiface.IAMAPI = nil
	mockSTSClient stsiface.STSAPI = nil
)

// getRawClientConfig creates a aws-sdk-go config, which is used to create client
// that can interact with AWS API. This builds credentials in the following
// order of preference:
//
// * Static credentials from 'config/client'
// * Environment variables
// * Instance metadata role
func (b *backend) getRawClientConfig(ctx context.Context, s logical.Storage, region, clientType string) (*aws.Config, error) {
	credsConfig := &awsutil.CredentialsConfig{
		Region: region,
	}

	// Read the configured secret key and access key
	config, err := b.nonLockedClientConfigEntry(ctx, s)
	if err != nil {
		return nil, err
	}

	endpoint := aws.String("")
	var maxRetries int = aws.UseServiceDefaultRetries
	if config != nil {
		// Override the defaults with configured values.
		switch {
		case clientType == "ec2" && config.Endpoint != "":
			endpoint = aws.String(config.Endpoint)
		case clientType == "iam" && config.IAMEndpoint != "":
			endpoint = aws.String(config.IAMEndpoint)
		case clientType == "sts":
			if config.STSEndpoint != "" {
				endpoint = aws.String(config.STSEndpoint)
			}
			if config.STSRegion != "" {
				region = config.STSRegion
			}
		}

		credsConfig.AccessKey = config.AccessKey
		credsConfig.SecretKey = config.SecretKey
		maxRetries = config.MaxRetries
	}

	credsConfig.HTTPClient = cleanhttp.DefaultClient()

	creds, err := credsConfig.GenerateCredentialChain()
	if err != nil {
		return nil, err
	}
	if creds == nil {
		return nil, fmt.Errorf("could not compile valid credential providers from static config, environment, shared, or instance metadata")
	}

	// Create a config that can be used to make the API calls.
	return &aws.Config{
		Credentials: creds,
		Region:      aws.String(region),
		HTTPClient:  cleanhttp.DefaultClient(),
		Endpoint:    endpoint,
		MaxRetries:  aws.Int(maxRetries),
	}, nil
}

// getClientConfig returns an aws-sdk-go config, with optionally assumed credentials
// It uses getRawClientConfig to obtain config for the runtime environment, and if
// stsRole is a non-empty string, it will use AssumeRole to obtain a set of assumed
// credentials. The credentials will expire after 15 minutes but will auto-refresh.
func (b *backend) getClientConfig(ctx context.Context, s logical.Storage, region, stsRole, accountID, clientType string) (*aws.Config, error) {

	config, err := b.getRawClientConfig(ctx, s, region, clientType)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return nil, fmt.Errorf("could not compile valid credentials through the default provider chain")
	}

	stsConfig, err := b.getRawClientConfig(ctx, s, region, "sts")
	if stsConfig == nil {
		return nil, fmt.Errorf("could not configure STS client")
	}
	if err != nil {
		return nil, err
	}
	if stsRole != "" {
		sess, err := session.NewSession(stsConfig)
		if err != nil {
			return nil, err
		}
		assumedCredentials := stscreds.NewCredentials(sess, stsRole)
		// Test that we actually have permissions to assume the role
		if _, err = assumedCredentials.Get(); err != nil {
			return nil, err
		}
		config.Credentials = assumedCredentials
	} else {
		if b.defaultAWSAccountID == "" {
			sess, err := session.NewSession(stsConfig)
			if err != nil {
				return nil, err
			}
			client := newSTSClient(sess)
			if client == nil {
				return nil, errwrap.Wrapf("could not obtain sts client: {{err}}", err)
			}
			inputParams := &sts.GetCallerIdentityInput{}
			identity, err := client.GetCallerIdentity(inputParams)
			if err != nil {
				return nil, errwrap.Wrapf("unable to fetch current caller: {{err}}", err)
			}
			if identity == nil {
				return nil, fmt.Errorf("got nil result from GetCallerIdentity")
			}
			b.defaultAWSAccountID = *identity.Account
		}
		if b.defaultAWSAccountID != accountID {
			return nil, fmt.Errorf("unable to fetch client for account ID %q -- default client is for account %q", accountID, b.defaultAWSAccountID)
		}
	}

	return config, nil
}

// flushCachedEC2Clients deletes all the cached ec2 client objects from the backend.
// If the client credentials configuration is deleted or updated in the backend, all
// the cached EC2 client objects will be flushed. Config mutex lock should be
// acquired for write operation before calling this method.
func (b *backend) flushCachedEC2Clients() {
	// deleting items in map during iteration is safe
	for region, _ := range b.EC2ClientsMap {
		delete(b.EC2ClientsMap, region)
	}
}

// flushCachedIAMClients deletes all the cached iam client objects from the
// backend. If the client credentials configuration is deleted or updated in
// the backend, all the cached IAM client objects will be flushed. Config mutex
// lock should be acquired for write operation before calling this method.
func (b *backend) flushCachedIAMClients() {
	// deleting items in map during iteration is safe
	for region, _ := range b.IAMClientsMap {
		delete(b.IAMClientsMap, region)
	}
}

// Gets an entry out of the user ID cache
func (b *backend) getCachedUserId(userId string) string {
	if userId == "" {
		return ""
	}
	if entry, ok := b.iamUserIdToArnCache.Get(userId); ok {
		b.iamUserIdToArnCache.SetDefault(userId, entry)
		return entry.(string)
	}
	return ""
}

// Sets an entry in the user ID cache
func (b *backend) setCachedUserId(userId, arn string) {
	if userId != "" {
		b.iamUserIdToArnCache.SetDefault(userId, arn)
	}
}

func (b *backend) stsRoleForAccount(ctx context.Context, s logical.Storage, accountID string) (string, error) {
	// Check if an STS configuration exists for the AWS account
	sts, err := b.lockedAwsStsEntry(ctx, s, accountID)
	if err != nil {
		return "", errwrap.Wrapf(fmt.Sprintf("error fetching STS config for account ID %q: {{err}}", accountID), err)
	}
	// An empty STS role signifies the master account
	if sts != nil {
		return sts.StsRole, nil
	}
	return "", nil
}

// clientEC2 creates a client to interact with AWS EC2 API
func (b *backend) clientEC2(ctx context.Context, s logical.Storage, region, accountID string) (ec2iface.EC2API, error) {
	stsRole, err := b.stsRoleForAccount(ctx, s, accountID)
	if err != nil {
		return nil, err
	}
	b.configMutex.RLock()
	if b.EC2ClientsMap[region] != nil && b.EC2ClientsMap[region][stsRole] != nil {
		defer b.configMutex.RUnlock()
		// If the client object was already created, return it
		return b.EC2ClientsMap[region][stsRole], nil
	}

	// Release the read lock and acquire the write lock
	b.configMutex.RUnlock()
	b.configMutex.Lock()
	defer b.configMutex.Unlock()

	// If the client gets created while switching the locks, return it
	if b.EC2ClientsMap[region] != nil && b.EC2ClientsMap[region][stsRole] != nil {
		return b.EC2ClientsMap[region][stsRole], nil
	}

	// Create an AWS config object using a chain of providers
	var awsConfig *aws.Config
	awsConfig, err = b.getClientConfig(ctx, s, region, stsRole, accountID, "ec2")

	if err != nil {
		return nil, err
	}

	if awsConfig == nil {
		return nil, fmt.Errorf("could not retrieve valid assumed credentials")
	}

	// Create a new EC2 client object, cache it and return the same
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, err
	}
	client := newEC2Client(sess)
	if client == nil {
		return nil, fmt.Errorf("could not obtain ec2 client")
	}
	if _, ok := b.EC2ClientsMap[region]; !ok {
		b.EC2ClientsMap[region] = map[string]ec2iface.EC2API{stsRole: client}
	} else {
		b.EC2ClientsMap[region][stsRole] = client
	}

	return b.EC2ClientsMap[region][stsRole], nil
}

// clientIAM creates a client to interact with AWS IAM API
func (b *backend) clientIAM(ctx context.Context, s logical.Storage, region, accountID string) (iamiface.IAMAPI, error) {
	stsRole, err := b.stsRoleForAccount(ctx, s, accountID)
	if err != nil {
		return nil, err
	}
	if stsRole == "" {
		b.Logger().Debug(fmt.Sprintf("no stsRole found for %s", accountID))
	} else {
		b.Logger().Debug(fmt.Sprintf("found stsRole %s for account %s", stsRole, accountID))
	}
	b.configMutex.RLock()
	if b.IAMClientsMap[region] != nil && b.IAMClientsMap[region][stsRole] != nil {
		defer b.configMutex.RUnlock()
		// If the client object was already created, return it
		b.Logger().Debug(fmt.Sprintf("returning cached client for region %s and stsRole %s", region, stsRole))
		return b.IAMClientsMap[region][stsRole], nil
	}
	b.Logger().Debug(fmt.Sprintf("no cached client for region %s and stsRole %s", region, stsRole))

	// Release the read lock and acquire the write lock
	b.configMutex.RUnlock()
	b.configMutex.Lock()
	defer b.configMutex.Unlock()

	// If the client gets created while switching the locks, return it
	if b.IAMClientsMap[region] != nil && b.IAMClientsMap[region][stsRole] != nil {
		return b.IAMClientsMap[region][stsRole], nil
	}

	// Create an AWS config object using a chain of providers
	var awsConfig *aws.Config
	awsConfig, err = b.getClientConfig(ctx, s, region, stsRole, accountID, "iam")

	if err != nil {
		return nil, err
	}

	if awsConfig == nil {
		return nil, fmt.Errorf("could not retrieve valid assumed credentials")
	}

	// Create a new IAM client object, cache it and return the same
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, err
	}
	client := newIAMClient(sess)
	if client == nil {
		return nil, fmt.Errorf("could not obtain iam client")
	}
	if _, ok := b.IAMClientsMap[region]; !ok {
		b.IAMClientsMap[region] = map[string]iamiface.IAMAPI{stsRole: client}
	} else {
		b.IAMClientsMap[region][stsRole] = client
	}
	return b.IAMClientsMap[region][stsRole], nil
}

// newEC2Client should be used instead of using ec2.New()
// directly because it allows us to mock out the EC2 client
// as needed for testing.
func newEC2Client(sess *session.Session) ec2iface.EC2API {
	if mockEC2Client != nil {
		return &replacableEC2Client{
			EC2API: mockEC2Client,
		}
	}
	return &replacableEC2Client{
		EC2API: ec2.New(sess),
	}
}

type replacableEC2Client struct {
	ec2iface.EC2API
}

// newIAMClient should be used instead of using iam.New()
// directly because it allows us to mock out the IAM client
// as needed for testing.
func newIAMClient(sess *session.Session) iamiface.IAMAPI {
	if mockIAMClient != nil {
		return &replacableIAMClient{
			IAMAPI: mockIAMClient,
		}
	}
	return &replacableIAMClient{
		IAMAPI: iam.New(sess),
	}
}

type replacableIAMClient struct {
	iamiface.IAMAPI
}

// newSTSClient should be used instead of using sts.New()
// directly because it allows us to mock out the STS client
// as needed for testing.
func newSTSClient(sess *session.Session) stsiface.STSAPI {
	if mockSTSClient != nil {
		return &replacableSTSClient{
			STSAPI: mockSTSClient,
		}
	}
	return &replacableSTSClient{
		STSAPI: sts.New(sess),
	}
}

type replacableSTSClient struct {
	stsiface.STSAPI
}
