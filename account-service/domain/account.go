package domain

// Account is a struct that represents an account in the domain.
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TenantID string `json:"tenant_id"`
}