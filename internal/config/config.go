package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/a8m/envsubst"
	"gopkg.in/yaml.v3"
)

const (
	DefaultAdminUser     = "admin@wgportal.local"
	DefaultAdminPassword = "wgportal-default"
)

// Config is the main configuration struct.
type Config struct {
	Core struct {
		// AdminUser defines the default administrator account that will be created
		AdminUserDisabled bool   `yaml:"disable_admin_user"`
		AdminUser         string `yaml:"admin_user"`
		AdminPassword     string `yaml:"admin_password"`
		AdminApiToken     string `yaml:"admin_api_token"` // if set, the API access is enabled automatically

		EditableKeys                         bool `yaml:"editable_keys"`
		CreateDefaultPeer                    bool `yaml:"create_default_peer"`             // DEPRECATED: in favor of CreateDefaultPeerOnLogin
		CreateDefaultPeerOnCreation          bool `yaml:"create_default_peer_on_creation"` // DEPRECATED: in favor of CreateDefaultPeerOnUserCreation
		CreateDefaultPeerOnLogin             bool `yaml:"create_default_peer_on_login"`
		CreateDefaultPeerOnUserCreation      bool `yaml:"create_default_peer_on_user_creation"`
		CreateDefaultPeerOnInterfaceCreation bool `yaml:"create_default_peer_on_interface_creation"`
		ReEnablePeerAfterUserEnable          bool `yaml:"re_enable_peer_after_user_enable"`
		DeletePeerAfterUserDeleted           bool `yaml:"delete_peer_after_user_deleted"`
		SelfProvisioningAllowed              bool `yaml:"self_provisioning_allowed"`
		ImportExisting                       bool `yaml:"import_existing"`
		RestoreState                         bool `yaml:"restore_state"`
	} `yaml:"core"`

	Advanced struct {
		LogLevel                 string        `yaml:"log_level"`
		LogPretty                bool          `yaml:"log_pretty"`
		LogJson                  bool          `yaml:"log_json"`
		StartListenPort          int           `yaml:"start_listen_port"`
		StartCidrV4              string        `yaml:"start_cidr_v4"`
		StartCidrV6              string        `yaml:"start_cidr_v6"`
		UseIpV6                  bool          `yaml:"use_ip_v6"`
		ConfigStoragePath        string        `yaml:"config_storage_path"` // keep empty to disable config export to file
		ExpiryCheckInterval      time.Duration `yaml:"expiry_check_interval"`
		RulePrioOffset           int           `yaml:"rule_prio_offset"`
		RouteTableOffset         int           `yaml:"route_table_offset"`
		ApiAdminOnly             bool          `yaml:"api_admin_only"` // if true, only admin users can access the API
		LimitAdditionalUserPeers int           `yaml:"limit_additional_user_peers"`
		StartupTimeout           time.Duration `yaml:"startup_timeout"`
	} `yaml:"advanced"`

	Backend Backend `yaml:"backend"`

	Statistics struct {
		UsePingChecks          bool          `yaml:"use_ping_checks"`
		PingCheckWorkers       int           `yaml:"ping_check_workers"`
		PingUnprivileged       bool          `yaml:"ping_unprivileged"`
		PingCheckInterval      time.Duration `yaml:"ping_check_interval"`
		DataCollectionInterval time.Duration `yaml:"data_collection_interval"`
		CollectInterfaceData   bool          `yaml:"collect_interface_data"`
		CollectPeerData        bool          `yaml:"collect_peer_data"`
		CollectAuditData       bool          `yaml:"collect_audit_data"`
		ListeningAddress       string        `yaml:"listening_address"`
	} `yaml:"statistics"`

	Mail MailConfig `yaml:"mail"`

	Auth Auth `yaml:"auth"`

	Database DatabaseConfig `yaml:"database"`

	Web WebConfig `yaml:"web"`

	Webhook WebhookConfig `yaml:"webhook"`

	Ha HaConfig `yaml:"ha"`
}

// LogStartupValues logs the startup values of the configuration in debug level
func (c *Config) LogStartupValues() {
	slog.Info("Configuration loaded!", "logLevel", c.Advanced.LogLevel)

	slog.Debug("Config Features",
		"editableKeys", c.Core.EditableKeys,
		"createDefaultPeerOnCreation", c.Core.CreateDefaultPeerOnUserCreation,
		"reEnablePeerAfterUserEnable", c.Core.ReEnablePeerAfterUserEnable,
		"deletePeerAfterUserDeleted", c.Core.DeletePeerAfterUserDeleted,
		"selfProvisioningAllowed", c.Core.SelfProvisioningAllowed,
		"limitAdditionalUserPeers", c.Advanced.LimitAdditionalUserPeers,
		"importExisting", c.Core.ImportExisting,
		"restoreState", c.Core.RestoreState,
		"useIpV6", c.Advanced.UseIpV6,
		"collectInterfaceData", c.Statistics.CollectInterfaceData,
		"collectPeerData", c.Statistics.CollectPeerData,
		"collectAuditData", c.Statistics.CollectAuditData,
	)

	slog.Debug("Config Settings",
		"configStoragePath", c.Advanced.ConfigStoragePath,
		"externalUrl", c.Web.ExternalUrl,
	)

	slog.Debug("Config Authentication",
		"oidcProviders", len(c.Auth.OpenIDConnect),
		"oauthProviders", len(c.Auth.OAuth),
		"ldapProviders", len(c.Auth.Ldap),
		"webauthnEnabled", c.Auth.WebAuthn.Enabled,
		"minPasswordLength", c.Auth.MinPasswordLength,
		"hideLoginForm", c.Auth.HideLoginForm,
	)

	slog.Debug("Config Backend",
		"defaultBackend", c.Backend.Default,
		"extraBackends", len(c.Backend.Mikrotik),
	)

}

// DefaultPeerCreationEnabled returns true if at least one default peer generation mechanism is enabled.
func (c *Config) DefaultPeerCreationEnabled() bool {
	return c.Core.CreateDefaultPeerOnLogin ||
		c.Core.CreateDefaultPeerOnInterfaceCreation ||
		c.Core.CreateDefaultPeerOnUserCreation
}

// defaultConfig returns the default configuration
func defaultConfig() *Config {
	cfg := &Config{}

	cfg.Core.AdminUserDisabled = getEnvBool("WG_PORTAL_CORE_DISABLE_ADMIN_USER", false)
	cfg.Core.AdminUser = getEnvStr("WG_PORTAL_CORE_ADMIN_USER", DefaultAdminUser)
	cfg.Core.AdminPassword = getEnvStr("WG_PORTAL_CORE_ADMIN_PASSWORD", DefaultAdminPassword)
	cfg.Core.AdminApiToken = getEnvStr("WG_PORTAL_CORE_ADMIN_API_TOKEN", "") // by default, the API access is disabled
	cfg.Core.ImportExisting = getEnvBool("WG_PORTAL_CORE_IMPORT_EXISTING", true)
	cfg.Core.RestoreState = getEnvBool("WG_PORTAL_CORE_RESTORE_STATE", true)
	cfg.Core.CreateDefaultPeer = getEnvBool("WG_PORTAL_CORE_CREATE_DEFAULT_PEER", false) // deprecated
	cfg.Core.CreateDefaultPeerOnCreation = getEnvBool("WG_PORTAL_CORE_CREATE_DEFAULT_PEER_ON_CREATION",
		false) // deprecated
	cfg.Core.CreateDefaultPeerOnLogin = getEnvBool("WG_PORTAL_CORE_CREATE_DEFAULT_PEER", false)
	cfg.Core.CreateDefaultPeerOnUserCreation = getEnvBool("WG_PORTAL_CORE_CREATE_DEFAULT_PEER_ON_USER_CREATION", false)
	cfg.Core.CreateDefaultPeerOnInterfaceCreation = getEnvBool("WG_PORTAL_CORE_CREATE_DEFAULT_PEER_ON_INTERFACE_CREATION",
		false)
	cfg.Core.EditableKeys = getEnvBool("WG_PORTAL_CORE_EDITABLE_KEYS", true)
	cfg.Core.SelfProvisioningAllowed = getEnvBool("WG_PORTAL_CORE_SELF_PROVISIONING_ALLOWED", false)
	cfg.Core.ReEnablePeerAfterUserEnable = getEnvBool("WG_PORTAL_CORE_RE_ENABLE_PEER_AFTER_USER_ENABLE", true)
	cfg.Core.DeletePeerAfterUserDeleted = getEnvBool("WG_PORTAL_CORE_DELETE_PEER_AFTER_USER_DELETED", false)

	cfg.Database = DatabaseConfig{
		Debug:                getEnvBool("WG_PORTAL_DATABASE_DEBUG", false),
		SlowQueryThreshold:   getEnvDuration("WG_PORTAL_DATABASE_SLOW_QUERY_THRESHOLD", 0),
		Type:                 SupportedDatabase(getEnvStr("WG_PORTAL_DATABASE_TYPE", "sqlite")),
		DSN:                  getEnvStr("WG_PORTAL_DATABASE_DSN", "data/sqlite.db"),
		EncryptionPassphrase: getEnvStr("WG_PORTAL_DATABASE_ENCRYPTION_PASSPHRASE", ""),
	}

	cfg.Backend = Backend{
		Default:                LocalBackendName, // local backend is the default (using wgcrtl)
		ReKeyTimeoutInterval:   getEnvDuration("WG_PORTAL_BACKEND_REKEY_TIMEOUT_INTERVAL", 180*time.Second),
		IgnoredLocalInterfaces: getEnvStrSlice("WG_PORTAL_BACKEND_IGNORED_LOCAL_INTERFACES", nil),
		// Most resolconf implementations use "tun." as a prefix for interface names.
		// But systemd's implementation uses no prefix, for example.
		LocalResolvconfPrefix: getEnvStr("WG_PORTAL_BACKEND_LOCAL_RESOLVCONF_PREFIX", "tun."),
	}

	cfg.Web = WebConfig{
		RequestLogging:    getEnvBool("WG_PORTAL_WEB_REQUEST_LOGGING", false),
		ExposeHostInfo:    getEnvBool("WG_PORTAL_WEB_EXPOSE_HOST_INFO", false),
		ExternalUrl:       getEnvStr("WG_PORTAL_WEB_EXTERNAL_URL", "http://localhost:8888"),
		BasePath:          getEnvStr("WG_PORTAL_WEB_BASE_PATH", ""),
		ListeningAddress:  getEnvStr("WG_PORTAL_WEB_LISTENING_ADDRESS", ":8888"),
		SessionIdentifier: getEnvStr("WG_PORTAL_WEB_SESSION_IDENTIFIER", "wgPortalSession"),
		SessionSecret:     getEnvStr("WG_PORTAL_WEB_SESSION_SECRET", "very_secret"),
		CsrfSecret:        getEnvStr("WG_PORTAL_WEB_CSRF_SECRET", "extremely_secret"),
		SiteTitle:         getEnvStr("WG_PORTAL_WEB_SITE_TITLE", "WireGuard Portal"),
		SiteCompanyName:   getEnvStr("WG_PORTAL_WEB_SITE_COMPANY_NAME", "WireGuard Portal"),
		CertFile:          getEnvStr("WG_PORTAL_WEB_CERT_FILE", ""),
		KeyFile:           getEnvStr("WG_PORTAL_WEB_KEY_FILE", ""),
		FrontendFilePath:  getEnvStr("WG_PORTAL_WEB_FRONTEND_FILEPATH", ""),
	}

	cfg.Advanced.LogLevel = getEnvStr("WG_PORTAL_ADVANCED_LOG_LEVEL", "info")
	cfg.Advanced.LogPretty = getEnvBool("WG_PORTAL_ADVANCED_LOG_PRETTY", false)
	cfg.Advanced.LogJson = getEnvBool("WG_PORTAL_ADVANCED_LOG_JSON", false)
	cfg.Advanced.StartListenPort = getEnvInt("WG_PORTAL_ADVANCED_START_LISTEN_PORT", 51820)
	cfg.Advanced.StartCidrV4 = getEnvStr("WG_PORTAL_ADVANCED_START_CIDR_V4", "10.11.12.0/24")
	cfg.Advanced.StartCidrV6 = getEnvStr("WG_PORTAL_ADVANCED_START_CIDR_V6", "fdfd:d3ad:c0de:1234::0/64")
	cfg.Advanced.UseIpV6 = getEnvBool("WG_PORTAL_ADVANCED_USE_IP_V6", true)
	cfg.Advanced.ConfigStoragePath = getEnvStr("WG_PORTAL_ADVANCED_CONFIG_STORAGE_PATH", "")
	cfg.Advanced.ExpiryCheckInterval = getEnvDuration("WG_PORTAL_ADVANCED_EXPIRY_CHECK_INTERVAL", 15*time.Minute)
	cfg.Advanced.RulePrioOffset = getEnvInt("WG_PORTAL_ADVANCED_RULE_PRIO_OFFSET", 20000)
	cfg.Advanced.RouteTableOffset = getEnvInt("WG_PORTAL_ADVANCED_ROUTE_TABLE_OFFSET", 20000)
	cfg.Advanced.ApiAdminOnly = getEnvBool("WG_PORTAL_ADVANCED_API_ADMIN_ONLY", true)
	cfg.Advanced.LimitAdditionalUserPeers = getEnvInt("WG_PORTAL_ADVANCED_LIMIT_ADDITIONAL_USER_PEERS", 0)
	cfg.Advanced.StartupTimeout = getEnvDuration("WG_PORTAL_ADVANCED_STARTUP_TIMEOUT", 5*time.Minute)

	cfg.Statistics.UsePingChecks = getEnvBool("WG_PORTAL_STATISTICS_USE_PING_CHECKS", true)
	cfg.Statistics.PingCheckWorkers = getEnvInt("WG_PORTAL_STATISTICS_PING_CHECK_WORKERS", 10)
	cfg.Statistics.PingUnprivileged = getEnvBool("WG_PORTAL_STATISTICS_PING_UNPRIVILEGED", false)
	cfg.Statistics.PingCheckInterval = getEnvDuration("WG_PORTAL_STATISTICS_PING_CHECK_INTERVAL", 1*time.Minute)
	cfg.Statistics.DataCollectionInterval = getEnvDuration("WG_PORTAL_STATISTICS_DATA_COLLECTION_INTERVAL",
		1*time.Minute)
	cfg.Statistics.CollectInterfaceData = getEnvBool("WG_PORTAL_STATISTICS_COLLECT_INTERFACE_DATA", true)
	cfg.Statistics.CollectPeerData = getEnvBool("WG_PORTAL_STATISTICS_COLLECT_PEER_DATA", true)
	cfg.Statistics.CollectAuditData = getEnvBool("WG_PORTAL_STATISTICS_COLLECT_AUDIT_DATA", true)
	cfg.Statistics.ListeningAddress = getEnvStr("WG_PORTAL_STATISTICS_LISTENING_ADDRESS", ":8787")

	cfg.Mail = MailConfig{
		Host:           getEnvStr("WG_PORTAL_MAIL_HOST", "127.0.0.1"),
		Port:           getEnvInt("WG_PORTAL_MAIL_PORT", 25),
		Encryption:     MailEncryption(getEnvStr("WG_PORTAL_MAIL_ENCRYPTION", string(MailEncryptionNone))),
		CertValidation: getEnvBool("WG_PORTAL_MAIL_CERT_VALIDATION", true),
		Username:       getEnvStr("WG_PORTAL_MAIL_USERNAME", ""),
		Password:       getEnvStr("WG_PORTAL_MAIL_PASSWORD", ""),
		AuthType:       MailAuthType(getEnvStr("WG_PORTAL_MAIL_AUTH_TYPE", string(MailAuthPlain))),
		From:           getEnvStr("WG_PORTAL_MAIL_FROM", "Wireguard Portal <noreply@wireguard.local>"),
		LinkOnly:       getEnvBool("WG_PORTAL_MAIL_LINK_ONLY", false),
		AllowPeerEmail: getEnvBool("WG_PORTAL_MAIL_ALLOW_PEER_EMAIL", false),
		TemplatesPath:  getEnvStr("WG_PORTAL_MAIL_TEMPLATES_PATH", ""),
	}

	cfg.Webhook.Url = getEnvStr("WG_PORTAL_WEBHOOK_URL", "") // no webhook by default
	cfg.Webhook.Authentication = getEnvStr("WG_PORTAL_WEBHOOK_AUTHENTICATION", "")
	cfg.Webhook.Timeout = getEnvDuration("WG_PORTAL_WEBHOOK_TIMEOUT", 10*time.Second)

	hostname, _ := os.Hostname()
	cfg.Ha = HaConfig{
		Role:                HaRole(getEnvStr("WG_PORTAL_HA_ROLE", string(HaRoleStandalone))),
		NodeId:              getEnvStr("WG_PORTAL_HA_NODE_ID", hostname),
		PublicIp:            getEnvStr("WG_PORTAL_HA_PUBLIC_IP", ""),
		Priority:            getEnvInt("WG_PORTAL_HA_PRIORITY", 0),
		DataDir:             getEnvStr("WG_PORTAL_HA_DATA_DIR", "data/ha"),
		HeartbeatInterval:   getEnvDuration("WG_PORTAL_HA_HEARTBEAT_INTERVAL", 10*time.Second),
		ReconcileInterval:   getEnvDuration("WG_PORTAL_HA_RECONCILE_INTERVAL", 30*time.Second),
		ProbeInterval:       getEnvDuration("WG_PORTAL_HA_PROBE_INTERVAL", 30*time.Second),
		ProbeTimeout:        getEnvDuration("WG_PORTAL_HA_PROBE_TIMEOUT", 15*time.Second),
		ProbeUrl:            getEnvStr("WG_PORTAL_HA_PROBE_URL", "https://api.ipify.org"),
		FailThreshold:       getEnvInt("WG_PORTAL_HA_FAIL_THRESHOLD", 3),
		HeartbeatTimeout:    getEnvDuration("WG_PORTAL_HA_HEARTBEAT_TIMEOUT", 60*time.Second),
		MinFailoverInterval: getEnvDuration("WG_PORTAL_HA_MIN_FAILOVER_INTERVAL", 5*time.Minute),
		AutoFailover:        getEnvBool("WG_PORTAL_HA_AUTO_FAILOVER", true),
		OnPromote:           getEnvStr("WG_PORTAL_HA_ON_PROMOTE", ""),
		OrchestratorUrl:     getEnvStr("WG_PORTAL_HA_ORCHESTRATOR_URL", ""),
		JoinToken:           getEnvStr("WG_PORTAL_HA_JOIN_TOKEN", ""),
		Galera: HaGaleraConfig{
			ClusterName: getEnvStr("WG_PORTAL_HA_GALERA_CLUSTER_NAME", "wg-portal"),
			SstUser:     getEnvStr("WG_PORTAL_HA_GALERA_SST_USER", "mariabackup"),
			SstPassword: getEnvStr("WG_PORTAL_HA_GALERA_SST_PASSWORD", ""),
		},
		Dns: HaDnsConfig{RegRu: HaRegRuConfig{
			ApiUrl:    getEnvStr("WG_PORTAL_HA_DNS_REGRU_API_URL", "https://api.reg.ru/api/regru2/"),
			Username:  getEnvStr("WG_PORTAL_HA_DNS_REGRU_USERNAME", ""),
			Password:  getEnvStr("WG_PORTAL_HA_DNS_REGRU_PASSWORD", ""),
			Zone:      getEnvStr("WG_PORTAL_HA_DNS_REGRU_ZONE", ""),
			Subdomain: getEnvStr("WG_PORTAL_HA_DNS_REGRU_SUBDOMAIN", ""),
		}},
	}

	cfg.Auth.WebAuthn.Enabled = getEnvBool("WG_PORTAL_AUTH_WEBAUTHN_ENABLED", true)
	cfg.Auth.MinPasswordLength = getEnvInt("WG_PORTAL_AUTH_MIN_PASSWORD_LENGTH", 16)
	cfg.Auth.HideLoginForm = getEnvBool("WG_PORTAL_AUTH_HIDE_LOGIN_FORM", false)

	return cfg
}

// GetConfig returns the configuration from the config file.
// Environment variable substitution is supported.
func GetConfig() (*Config, error) {
	cfg := defaultConfig()

	// override config values from YAML file

	cfgFileName := "config/config.yaml"
	cfgFileNameFallback := "config/config.yml"
	if envCfgFileName := os.Getenv("WG_PORTAL_CONFIG"); envCfgFileName != "" {
		cfgFileName = envCfgFileName
		cfgFileNameFallback = envCfgFileName
	}

	// check if the config file exists, otherwise use the fallback file name
	if _, err := os.Stat(cfgFileName); os.IsNotExist(err) {
		cfgFileName = cfgFileNameFallback
	}

	if err := loadConfigFile(cfg, cfgFileName); err != nil {
		return nil, fmt.Errorf("failed to load config from yaml: %w", err)
	}

	cfg.Web.Sanitize()
	err := cfg.Backend.Validate()
	if err != nil {
		return nil, err
	}
	for i := range cfg.Auth.Ldap {
		if err := cfg.Auth.Ldap[i].Sanitize(); err != nil {
			return nil, fmt.Errorf("sanitizing of ldap config for %s failed: %w", cfg.Auth.Ldap[i].ProviderName, err)
		}
	}

	if cfg.Ha.NodeId == "" { // compose passes unset variables as empty strings
		cfg.Ha.NodeId, _ = os.Hostname()
	}
	if err := cfg.Ha.Validate(); err != nil {
		return nil, err
	}
	applyHaRoleOverrides(cfg)

	handleDeprecatedConfigValues(cfg)

	return cfg, nil
}

// loadConfigFile loads the configuration from a YAML file into the given cfg struct.
func loadConfigFile(cfg any, filename string) error {
	data, err := envsubst.ReadFile(filename)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Warn("Config file not found, using default values", "filename", filename)
			return nil
		}
		return fmt.Errorf("envsubst error: %v", err)
	}

	err = yaml.Unmarshal(data, cfg)
	if err != nil {
		return fmt.Errorf("yaml error: %v", err)
	}

	return nil
}

func getEnvStr(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}

	return fallback
}

func getEnvStrSlice(name string, fallback []string) []string {
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}

	strParts := strings.Split(v, ",")
	stringSlice := make([]string, 0, len(strParts))

	for _, s := range strParts {
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			stringSlice = append(stringSlice, trimmed)
		}
	}

	return stringSlice
}

func getEnvBool(name string, fallback bool) bool {
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		slog.Warn("invalid bool env, using fallback", "env", name, "value", v, "fallback", fallback)
		return fallback
	}

	return b
}

func getEnvInt(name string, fallback int) int {
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}

	i, err := strconv.Atoi(v)
	if err != nil {
		slog.Warn("invalid int env, using fallback", "env", name, "value", v, "fallback", fallback)
		return fallback
	}

	return i
}

func getEnvDuration(name string, fallback time.Duration) time.Duration {
	v, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}

	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("invalid duration env, using fallback", "env", name, "value", v, "fallback", fallback)
		return fallback
	}

	return d
}

func handleDeprecatedConfigValues(cfg *Config) {
	// deprecated, will be removed in 2.4
	if cfg.Core.CreateDefaultPeer {
		slog.Warn("DEPRECATION WARNING: deprecated core config option: create_default_peer (WG_PORTAL_CORE_CREATE_DEFAULT_PEER)")
		cfg.Core.CreateDefaultPeerOnLogin = true
	}

	// deprecated, will be removed in 2.4
	if cfg.Core.CreateDefaultPeerOnCreation {
		slog.Warn("DEPRECATION WARNING: deprecated core config option: create_default_peer_on_creation (WG_PORTAL_CORE_CREATE_DEFAULT_PEER_ON_CREATION)")
		cfg.Core.CreateDefaultPeerOnUserCreation = true
		cfg.Core.CreateDefaultPeerOnInterfaceCreation = true
	}
}

// applyHaRoleOverrides switches off features that must not run for the configured HA role.
func applyHaRoleOverrides(cfg *Config) {
	switch cfg.Ha.Role {
	case HaRoleOrchestrator:
		// the orchestrator has no WireGuard interfaces, nodes own the kernel state
		cfg.Core.ImportExisting = false
		cfg.Core.RestoreState = false
		// statistics stay enabled: they are read from the replicated DB, main.go does not start the collector loops
	case HaRoleNode:
		// nodes apply the replicated DB via the reconcile loop and must never write interface state on startup
		cfg.Core.ImportExisting = false
		cfg.Core.RestoreState = false
		cfg.Core.AdminUserDisabled = true // users live in the replicated DB, created by the orchestrator
	}
}
