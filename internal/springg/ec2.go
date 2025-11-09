package springg

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

const (
	metadataTokenURL    = "http://169.254.169.254/latest/api/token"
	metadataInstanceURL = "http://169.254.169.254/latest/meta-data/instance-id"
	metadataLocalIPURL  = "http://169.254.169.254/latest/meta-data/local-ipv4"
	tokenTTL            = "21600" // 6 hours
)

// EC2Info holds EC2 instance metadata
type EC2Info struct {
	InstanceID string
	LocalIP    string
	ASGName    string
	Region     string
	IsInASG    bool
	PeerIPs    []string
	asgClient  *autoscaling.Client
	ec2Client  *ec2.Client
}

// DetectEC2 detects if running on EC2 and gathers instance metadata
func DetectEC2(ctx context.Context) (*EC2Info, error) {
	// Try to get EC2 metadata token
	token, err := getMetadataToken()
	if err != nil {
		// Not running on EC2
		return nil, nil
	}

	// Get instance ID
	instanceID, err := getMetadata(metadataInstanceURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to get instance ID: %w", err)
	}

	// Get local IP
	localIP, err := getMetadata(metadataLocalIPURL, token)
	if err != nil {
		return nil, fmt.Errorf("failed to get local IP: %w", err)
	}

	// Load AWS config
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	region := awsCfg.Region
	asgClient := autoscaling.NewFromConfig(awsCfg)
	ec2Client := ec2.NewFromConfig(awsCfg)

	info := &EC2Info{
		InstanceID: instanceID,
		LocalIP:    localIP,
		Region:     region,
		asgClient:  asgClient,
		ec2Client:  ec2Client,
	}

	// Check if part of ASG
	asgName, err := info.getASGName(ctx)
	if err == nil && asgName != "" {
		info.ASGName = asgName
		info.IsInASG = true
	}

	return info, nil
}

// getMetadataToken gets an IMDSv2 token
func getMetadataToken() (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}

	req, err := http.NewRequest("PUT", metadataTokenURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", tokenTTL)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(token), nil
}

// getMetadata gets EC2 instance metadata using IMDSv2
func getMetadata(url, token string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-aws-ec2-metadata-token", token)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(data)), nil
}

// getASGName gets the Auto Scaling Group name for this instance
func (e *EC2Info) getASGName(ctx context.Context) (string, error) {
	input := &autoscaling.DescribeAutoScalingInstancesInput{
		InstanceIds: []string{e.InstanceID},
	}

	result, err := e.asgClient.DescribeAutoScalingInstances(ctx, input)
	if err != nil {
		return "", err
	}

	if len(result.AutoScalingInstances) == 0 {
		return "", nil // Not in an ASG
	}

	return aws.ToString(result.AutoScalingInstances[0].AutoScalingGroupName), nil
}

// RefreshPeerIPs updates the list of peer IPs in the same ASG
func (e *EC2Info) RefreshPeerIPs(ctx context.Context) error {
	if !e.IsInASG {
		return nil
	}

	// Get instances in ASG
	input := &autoscaling.DescribeAutoScalingInstancesInput{}
	result, err := e.asgClient.DescribeAutoScalingInstances(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to describe ASG instances: %w", err)
	}

	// Filter instances in our ASG
	var instanceIDs []string
	for _, instance := range result.AutoScalingInstances {
		if aws.ToString(instance.AutoScalingGroupName) == e.ASGName {
			instanceIDs = append(instanceIDs, aws.ToString(instance.InstanceId))
		}
	}

	if len(instanceIDs) == 0 {
		return nil
	}

	// Get private IPs for these instances
	ec2Input := &ec2.DescribeInstancesInput{
		InstanceIds: instanceIDs,
	}

	ec2Result, err := e.ec2Client.DescribeInstances(ctx, ec2Input)
	if err != nil {
		return fmt.Errorf("failed to describe EC2 instances: %w", err)
	}

	// Extract private IPs
	var peerIPs []string
	for _, reservation := range ec2Result.Reservations {
		for _, instance := range reservation.Instances {
			privateIP := aws.ToString(instance.PrivateIpAddress)
			if privateIP != "" && privateIP != e.LocalIP {
				peerIPs = append(peerIPs, privateIP)
			}
		}
	}

	// Sort for deterministic ordering
	sort.Strings(peerIPs)
	e.PeerIPs = peerIPs

	return nil
}

// GetSeedPeers returns initial seed peers for gossip (first 3 peers)
func (e *EC2Info) GetSeedPeers() []string {
	if len(e.PeerIPs) <= 3 {
		return e.PeerIPs
	}
	return e.PeerIPs[:3]
}
