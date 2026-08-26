package googlegroup

type Config struct {
	// ServiceAccountKey is the service account JSON key. It is stored base64-encoded
	// in config/env and decoded during config loading.
	ServiceAccountKey string `yaml:"service_account_key"`

	// ImpersonateSubject is the Workspace admin the service account impersonates via
	// domain-wide delegation. The Admin SDK rejects service-account identities that
	// are not acting on behalf of a real admin user.
	ImpersonateSubject string `yaml:"impersonate_subject"`

	// CacheTTL is how long a fetched member list is reused, as a time.ParseDuration string.
	CacheTTL string `yaml:"cache_ttl"`

	// Domain is the Workspace domain the club's own groups live in. When set, the API
	// speaks bare group names -- "general" rather than "general@sdc.nycu.club" -- in
	// both directions. Leaving it empty keeps full addresses everywhere.
	Domain string `yaml:"domain"`

	// LoginGroup is the mailing list whose active members may sign in. Empty disables
	// login entirely rather than allowing everyone.
	LoginGroup string `yaml:"login_group"`
}
