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

// ConnectorTypeList struct for ConnectorTypeList
type ConnectorTypeList struct {
	Kind  string          `json:"kind"`
	Page  int32           `json:"page"`
	Size  int32           `json:"size"`
	Total int32           `json:"total"`
	Items []ConnectorType `json:"items"`
}
