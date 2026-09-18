package setting

type SystemConfig struct {
	Port int    `mapstructure:"port" json:"port" yaml:"port"`
	Mode string `mapstructure:"mode" json:"mode" yaml:"mode"`
	// SuperUser is the single bootstrap admin email seeded on startup. The
	// account is created (or promoted to the "super" role) on first boot.
	SuperUser         string `mapstructure:"super-user" json:"super-user" yaml:"super-user"`
	SuperUserPassword string `mapstructure:"super-user-password" json:"super-user-password" yaml:"super-user-password"`
	// EncryptKey is a base64-encoded 32-byte (AES-256) key used to encrypt
	// per-user secrets (object-storage and LLM credentials, the agent Daytona
	// key) at rest in Postgres. When empty, those secrets are persisted
	// unencrypted (still durable, but not encrypted) and a startup warning is
	// emitted. Set via ANTELOPE_SYSTEM_ENCRYPT_KEY; generate one with
	// `openssl rand -base64 32`.
	EncryptKey string `mapstructure:"encrypt-key" json:"encrypt-key" yaml:"encrypt-key"`
	// AllowPersonalStorage controls whether users may register their own
	// object-storage credentials in addition to the storage they inherit
	// through group membership. Deployments handling controlled data can turn
	// it off entirely; individual groups can also forbid it for their members
	// (Group.AllowPersonalStorage), and the two compose most-restrictive-wins.
	// Defaults to true. Set via ANTELOPE_SYSTEM_ALLOW_PERSONAL_STORAGE.
	AllowPersonalStorage *bool `mapstructure:"allow-personal-storage" json:"allow-personal-storage" yaml:"allow-personal-storage"`
}

// PersonalStorageAllowed reports the platform-level setting, defaulting to true
// when unset so existing deployments keep working after an upgrade.
func (s *SystemConfig) PersonalStorageAllowed() bool {
	if s.AllowPersonalStorage == nil {
		return true
	}
	return *s.AllowPersonalStorage
}

func (s *SystemConfig) GetGinMode() (mode string) {
	switch s.Mode {
	case "debug":
		mode = "debug"
	case "test":
		mode = "test"
	default:
		mode = "release"
	}
	return mode
}

// IsProduction returns true when the server is running in release mode.
// Used to enforce the Secure flag on cookies.
func (s *SystemConfig) IsProduction() bool {
	return s.GetGinMode() == "release"
}
