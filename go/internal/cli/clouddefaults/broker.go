package clouddefaults

import "github.com/wendylabsinc/wendy-go/cloud"

// BrokerURL resolves the Cloud session's broker address through shared policy.
func BrokerURL(cloudGRPC, brokerURL, defaultBrokerPort string) string {
	return cloud.BrokerURL(cloudGRPC, brokerURL, defaultBrokerPort)
}
