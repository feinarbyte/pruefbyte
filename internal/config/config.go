// Package config loads the layered pruefbyte configuration:
// built-in defaults < global file < repository file < PRUEFBYTE_* env vars.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RepoConfigFile is the file name looked up at the MR's base commit.
const RepoConfigFile = ".pruefbyte.yml"

type Config struct {
	GitLab GitLab `yaml:"gitlab"`
	LLM    LLM    `yaml:"llm"`
	OCR    OCR    `yaml:"ocr"`
	Review Review `yaml:"review"`
}

type GitLab struct {
	// URL of the GitLab instance. Defaults to CI_SERVER_URL.
	URL string `yaml:"url"`
	// TokenEnv names the env var holding the bot account's PAT.
	TokenEnv string `yaml:"token_env"`
}

type LLM struct {
	// Provider is an OCR built-in provider name or any other name for a custom provider.
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
	// APIKeyEnv names the env var holding the LLM API key.
	APIKeyEnv string `yaml:"api_key_env"`
	// URL overrides the built-in provider's base URL; required for custom providers.
	URL string `yaml:"url"`
	// Protocol is required for custom providers: anthropic|openai|openai-responses|anthropic-bedrock.
	Protocol     string            `yaml:"protocol"`
	TimeoutSec   int               `yaml:"timeout_sec"`
	ExtraBody    map[string]any    `yaml:"extra_body"`
	ExtraHeaders map[string]string `yaml:"extra_headers"`
	AWSRegion    string            `yaml:"aws_region"`
	AWSProfile   string            `yaml:"aws_profile"`
}

type OCR struct {
	Binary                       string        `yaml:"binary"`
	Effort                       string        `yaml:"effort"`
	Language                     string        `yaml:"language"`
	Timeout                      time.Duration `yaml:"timeout"`
	Concurrency                  int           `yaml:"concurrency"`
	MaxTokensBudget              int           `yaml:"max_tokens_budget"`
	Exclude                      []string      `yaml:"exclude"`
	Rules                        []Rule        `yaml:"rules"`
	RuleFile                     string        `yaml:"rule_file"`
	UseMRDescriptionAsBackground bool          `yaml:"use_mr_description_as_background"`
	ExtraArgs                    []string      `yaml:"extra_args"`
}

type Rule struct {
	Path string `yaml:"path" json:"path"`
	Rule string `yaml:"rule" json:"rule"`
	// MergeSystemRule keeps OCR's built-in language rule for matching files and
	// adds this one. Without it the entry replaces the built-in rule.
	MergeSystemRule bool `yaml:"merge_system_rule" json:"merge_system_rule,omitempty"`
}

type Review struct {
	MinSeverity     string   `yaml:"min_severity"`
	Categories      []string `yaml:"categories"`
	MaxComments     int      `yaml:"max_comments"`
	Suggestions     bool     `yaml:"suggestions"`
	ResolveOutdated bool     `yaml:"resolve_outdated"`
	FailOnSeverity  string   `yaml:"fail_on_severity"`
	PostFailures    bool     `yaml:"post_failures"`
}

// repoConfig lists the only keys a repository file may set. Anything that decides
// where secrets are sent or what gets executed (gitlab.*, llm endpoint/provider,
// ocr.binary, ocr.extra_args) stays under the operator's control.
type repoConfig struct {
	LLM struct {
		// Provider must be an OCR built-in: their endpoints are fixed by OCR, so the
		// repository can pick a vendor but cannot point the API key at its own URL.
		Provider string `yaml:"provider"`
		Model    string `yaml:"model"`
	} `yaml:"llm"`
	OCR struct {
		Effort                       string        `yaml:"effort"`
		Language                     string        `yaml:"language"`
		Timeout                      time.Duration `yaml:"timeout"`
		Concurrency                  int           `yaml:"concurrency"`
		MaxTokensBudget              int           `yaml:"max_tokens_budget"`
		Exclude                      []string      `yaml:"exclude"`
		Rules                        []Rule        `yaml:"rules"`
		RuleFile                     string        `yaml:"rule_file"`
		UseMRDescriptionAsBackground bool          `yaml:"use_mr_description_as_background"`
	} `yaml:"ocr"`
	Review Review `yaml:"review"`
}

func Default() Config {
	return Config{
		GitLab: GitLab{TokenEnv: "PRUEFBYTE_GITLAB_TOKEN"}, //nolint:gosec // G101: the name of an env var, not a credential
		LLM:    LLM{APIKeyEnv: "PRUEFBYTE_LLM_API_KEY"},    //nolint:gosec // G101: the name of an env var, not a credential
		OCR: OCR{
			Binary:                       "ocr",
			Effort:                       "medium",
			Timeout:                      30 * time.Minute,
			Concurrency:                  8,
			Exclude:                      []string{},
			UseMRDescriptionAsBackground: true,
		},
		Review: Review{
			MinSeverity:     "low",
			MaxComments:     30,
			Suggestions:     true,
			ResolveOutdated: true,
			PostFailures:    true,
		},
	}
}

// ApplyGlobal overlays an operator-controlled YAML document. All keys are allowed.
func (c *Config) ApplyGlobal(data []byte) error {
	return decodeStrict(data, c)
}

// ApplyRepo overlays a repository YAML document. Only keys in repoConfig are allowed;
// anything else is rejected so a merge request cannot redirect credentials.
func (c *Config) ApplyRepo(data []byte) error {
	var probe repoConfig
	if err := decodeStrict(data, &probe); err != nil {
		return fmt.Errorf("%s: %w (only llm.provider, llm.model, ocr.* and review.* are allowed here, except ocr.binary and ocr.extra_args)", RepoConfigFile, err)
	}
	if p := probe.LLM.Provider; p != "" {
		if !IsBuiltinProvider(p) {
			return fmt.Errorf("%s: llm.provider %q is not an OCR built-in provider; custom providers (llm.url, llm.protocol) can only be set in the global config", RepoConfigFile, p)
		}
		if p != c.LLM.Provider {
			// An endpoint configured for the global provider does not belong to this one.
			c.LLM.URL, c.LLM.Protocol = "", ""
		}
	}
	return decodeStrict(data, c)
}

func decodeStrict(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// LoadGlobalFile reads the global config file if path is non-empty.
func (c *Config) LoadGlobalFile(path string) error {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config %s: %w", path, err)
	}
	if err := c.ApplyGlobal(data); err != nil {
		return fmt.Errorf("parsing config %s: %w", path, err)
	}
	return nil
}

var severities = map[string]int{"low": 1, "medium": 2, "high": 3, "critical": 4}

// SeverityRank returns 1 (low) .. 4 (critical), or 0 for unknown values.
func SeverityRank(s string) int {
	return severities[strings.ToLower(strings.TrimSpace(s))]
}

var protocols = map[string]bool{"anthropic": true, "openai": true, "openai-responses": true, "anthropic-bedrock": true}

func (c Config) Validate() error {
	var errs []error
	if c.LLM.Provider == "" {
		errs = append(errs, errors.New("llm.provider is required: set it in .pruefbyte.yml or the global config"))
	}
	if c.LLM.Model == "" {
		errs = append(errs, errors.New("llm.model is required: set it in .pruefbyte.yml or the global config"))
	}
	if !IsBuiltinProvider(c.LLM.Provider) && c.LLM.Provider != "" {
		if !protocols[c.LLM.Protocol] {
			errs = append(errs, fmt.Errorf("llm.provider %q is not an OCR built-in provider, so llm.protocol must be one of anthropic, openai, openai-responses, anthropic-bedrock", c.LLM.Provider))
		}
		if c.LLM.URL == "" && c.LLM.Protocol != "anthropic-bedrock" {
			errs = append(errs, fmt.Errorf("llm.url is required for custom provider %q", c.LLM.Provider))
		}
	}
	for k, v := range c.LLM.ExtraHeaders {
		// ocr takes headers as one `k=v,k="v,w"` string and cannot represent these.
		if k == "" || strings.ContainsAny(k, `,="`) || strings.Contains(v, `"`) {
			errs = append(errs, fmt.Errorf("llm.extra_headers %q: ocr cannot pass a header whose name contains , = or \" or whose value contains \"", k))
		}
	}
	if c.GitLab.TokenEnv == "" {
		errs = append(errs, errors.New("gitlab.token_env is required"))
	}
	switch c.OCR.Effort {
	case "", "low", "medium", "high":
	default:
		errs = append(errs, fmt.Errorf("ocr.effort must be low, medium or high, got %q", c.OCR.Effort))
	}
	if c.OCR.RuleFile != "" && len(c.OCR.Rules) > 0 {
		errs = append(errs, errors.New("set either ocr.rules or ocr.rule_file, not both"))
	}
	for _, r := range c.OCR.Rules {
		if r.Path == "" || r.Rule == "" {
			errs = append(errs, errors.New("every ocr.rules entry needs path and rule"))
			break
		}
	}
	if SeverityRank(c.Review.MinSeverity) == 0 {
		errs = append(errs, fmt.Errorf("review.min_severity must be low, medium, high or critical, got %q", c.Review.MinSeverity))
	}
	if c.Review.FailOnSeverity != "" && SeverityRank(c.Review.FailOnSeverity) == 0 {
		errs = append(errs, fmt.Errorf("review.fail_on_severity must be empty or low, medium, high, critical, got %q", c.Review.FailOnSeverity))
	}
	if c.Review.MaxComments < 0 {
		errs = append(errs, errors.New("review.max_comments must be >= 0 (0 = unlimited)"))
	}
	return errors.Join(errs...)
}

// builtinProviders mirrors OCR's built-in provider presets (`ocr llm providers`,
// v1.12.10). OCR rejects a custom provider that uses one of these names.
var builtinProviders = map[string]bool{
	"anthropic": true, "bedrock": true, "openai": true, "openai-responses": true,
	"openrouter": true, "gemini": true, "dashscope": true, "dashscope-tokenplan": true,
	"volcengine": true, "deepseek": true, "tencent-tokenhub": true, "hy-tokenplan": true,
	"iflytek": true, "kimi": true, "kimi-global": true, "z-ai": true, "z-ai-coding": true,
	"mimo": true, "minimax": true, "minimax-cn": true, "baidu-qianfan": true,
	"siliconflow": true, "siliconflow-cn": true, "novita": true, "xai": true,
	"edenai": true, "litellm": true, "mistral": true, "ollama-cloud": true,
}

func IsBuiltinProvider(name string) bool { return builtinProviders[name] }

// YAML renders the effective config. Secrets are never stored in Config, so
// only the names of the env vars that hold them appear.
func (c Config) YAML() string {
	out, _ := yaml.Marshal(c)
	return string(out)
}
