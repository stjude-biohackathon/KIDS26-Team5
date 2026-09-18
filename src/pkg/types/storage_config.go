package types

// UserStorageConfigDto represents the user's storage configuration
type UserStorageConfigDto struct {
	Host               string `json:"host" binding:"required,nonblank"`
	Port               int    `json:"port" binding:"required,min=1,max=65535"`
	AccessKey          string `json:"access_key" binding:"required,nonblank"`
	SecretKey          string `json:"secret_key" binding:"required,nonblank"`
	UseSSL             bool   `json:"use_ssl"`
	Region             string `json:"region"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}

// StorageConfigSummary describes one storage configuration a user can reach.
// Deliberately carries no credentials: members of a group use its storage
// without ever seeing the keys behind it.
type StorageConfigSummary struct {
	ID   uint   `json:"id"`
	Name string `json:"name"`
	// OwnerType is "personal" or "group".
	OwnerType string `json:"owner_type"`
	// OwnerName is the owning group's display name; empty for personal configs.
	OwnerName      string `json:"owner_name,omitempty"`
	Classification string `json:"classification"`
	Endpoint       string `json:"endpoint,omitempty"`
	// Writable reports whether the caller may write, so the UI can disable
	// upload affordances rather than letting them fail at the API.
	Writable bool `json:"writable"`
	// Owned reports whether this is the caller's own personal configuration.
	// OwnerType alone cannot answer that: a personally registered config that
	// has been granted to a group still reads "personal" to every member who
	// reaches it, so without this the UI would offer them an edit form for
	// someone else's credentials.
	Owned bool `json:"owned"`
	// SharedVia names the caller's groups that grant them this config, which is
	// where their access actually comes from when they are not the owner.
	SharedVia []string `json:"shared_via,omitempty"`
}

// UserStorageConfigResponse represents the response for user storage config
// Note: SecretKey is masked for security
type UserStorageConfigResponse struct {
	ID                 uint   `json:"id,omitempty"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	AccessKey          string `json:"access_key"`
	SecretKey          string `json:"secret_key"` // Will be masked
	UseSSL             bool   `json:"use_ssl"`
	Region             string `json:"region"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	Configured         bool   `json:"configured"`
}

// TestStorageConnectionDto is the request to test storage connection
type TestStorageConnectionDto struct {
	Host               string `json:"host" binding:"required,nonblank"`
	Port               int    `json:"port" binding:"required,min=1,max=65535"`
	AccessKey          string `json:"access_key" binding:"required,nonblank"`
	SecretKey          string `json:"secret_key" binding:"required,nonblank"`
	UseSSL             bool   `json:"use_ssl"`
	Region             string `json:"region"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}
