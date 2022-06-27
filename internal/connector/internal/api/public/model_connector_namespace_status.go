/*
 * Connector Management API
 *
 * Connector Management API is a REST API to manage connectors.
 *
 * API version: 0.1.0
 * Contact: rhosak-support@redhat.com
 * Generated by: OpenAPI Generator (https://openapi-generator.tech)
 */

package public

// ConnectorNamespaceStatus struct for ConnectorNamespaceStatus
type ConnectorNamespaceStatus struct {
	State              ConnectorNamespaceState `json:"state"`
	Version            string                  `json:"version,omitempty"`
	ConnectorsDeployed int32                   `json:"connectors_deployed"`
	Error              string                  `json:"error,omitempty"`
}
