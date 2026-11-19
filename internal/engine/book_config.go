package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	DefaultMaxHeadingChars    = 42
	MaxBookConfigBytes        = 32 * 1024
	MaxBookTitleRunes         = 120
	MaxBookKickerRunes        = 80
	MaxHeadingCharsLimit      = 200
	MaxBannedWordsCount       = 100
	MaxBannedWordRunes        = 64
	MaxTimelineGuidanceRunes  = 2000
	MaxExtraLLMsMarkdownRunes = 16000
)

// BookConfigFile represents the optional repository-level editorial configuration
// stored at src/book.json. Pointer fields allow distinguishing omitted keys
// (inherit CLI/env defaults) from explicit overrides (false, 0, "", or []).
type BookConfigFile struct {
	Title             *string                 `json:"title,omitempty"`
	Kicker            *string                 `json:"kicker,omitempty"`
	HidePoweredBy     *bool                   `json:"hide_powered_by,omitempty"`
	Lint              *BookLintConfigFile     `json:"lint,omitempty"`
	Timeline          *BookTimelineConfigFile `json:"timeline,omitempty"`
	ExtraLLMsMarkdown *string                 `json:"extra_llms_markdown,omitempty"`
}

type BookLintConfigFile struct {
	MaxHeadingChars *int      `json:"max_heading_chars,omitempty"`
	ForbidEmDash    *bool     `json:"forbid_em_dash,omitempty"`
	BannedWords     *[]string `json:"banned_words,omitempty"`
}

type BookTimelineConfigFile struct {
	PromptGuidance *string `json:"prompt_guidance,omitempty"`
}

// EffectiveLintConfig holds the resolved runtime linter rules for chapter writes.
type EffectiveLintConfig struct {
	MaxHeadingChars int
	ForbidEmDash    bool
	BannedWords     []string
}

// EffectiveBookConfig holds the merged runtime book-level editorial configuration
// resolved from baseline CLI/env settings plus optional src/book.json overrides.
type EffectiveBookConfig struct {
	BookTitle         string
	BookKicker        string
	HidePoweredBy     bool
	Lint              EffectiveLintConfig
	TimelineGuidance  string
	ExtraLLMsMarkdown string
	FromRepoFile      bool
}

func defaultEffectiveBookConfig(base EngineConfig) EffectiveBookConfig {
	title := strings.TrimSpace(base.BookTitle)
	if title == "" {
		title = "Ghostbook"
	}
	kicker := strings.TrimSpace(base.BookKicker)
	if kicker == "" {
		kicker = "Living Book"
	}
	return EffectiveBookConfig{
		BookTitle:     title,
		BookKicker:    kicker,
		HidePoweredBy: base.HidePoweredBy,
		Lint: EffectiveLintConfig{
			MaxHeadingChars: DefaultMaxHeadingChars,
			ForbidEmDash:    true,
			BannedWords:     dedupeWords(base.BannedWords),
		},
		ExtraLLMsMarkdown: strings.TrimSpace(base.ExtraLLMsMarkdown),
		FromRepoFile:      false,
	}
}

func dedupeWords(words []string) []string {
	if len(words) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(words))
	var out []string
	for _, raw := range words {
		clean := strings.TrimSpace(raw)
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, clean)
	}
	return out
}

// ParseAndValidateBookConfig strictly decodes and validates src/book.json bytes.
func ParseAndValidateBookConfig(raw []byte) (*BookConfigFile, error) {
	if len(raw) > MaxBookConfigBytes {
		return nil, fmt.Errorf("%w: src/book.json exceeds %d bytes (%d bytes)", ErrInvalidArgument, MaxBookConfigBytes, len(raw))
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%w: src/book.json must be a JSON object", ErrInvalidArgument)
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: src/book.json must be a top-level JSON object", ErrInvalidArgument)
	}

	var cfg BookConfigFile
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%w: invalid src/book.json: %v", ErrInvalidArgument, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("%w: src/book.json contains unexpected trailing content", ErrInvalidArgument)
	}

	if cfg.Title != nil {
		if strings.ContainsAny(*cfg.Title, "\r\n") {
			return nil, fmt.Errorf("%w: title must be a single line", ErrInvalidArgument)
		}
		clean := strings.TrimSpace(*cfg.Title)
		if utf8.RuneCountInString(clean) > MaxBookTitleRunes {
			return nil, fmt.Errorf("%w: title exceeds %d characters", ErrInvalidArgument, MaxBookTitleRunes)
		}
		cfg.Title = &clean
	}

	if cfg.Kicker != nil {
		if strings.ContainsAny(*cfg.Kicker, "\r\n") {
			return nil, fmt.Errorf("%w: kicker must be a single line", ErrInvalidArgument)
		}
		clean := strings.TrimSpace(*cfg.Kicker)
		if utf8.RuneCountInString(clean) > MaxBookKickerRunes {
			return nil, fmt.Errorf("%w: kicker exceeds %d characters", ErrInvalidArgument, MaxBookKickerRunes)
		}
		cfg.Kicker = &clean
	}

	if cfg.Lint != nil {
		if cfg.Lint.MaxHeadingChars != nil {
			v := *cfg.Lint.MaxHeadingChars
			if v < 0 || v > MaxHeadingCharsLimit {
				return nil, fmt.Errorf("%w: lint.max_heading_chars must be between 0 and %d", ErrInvalidArgument, MaxHeadingCharsLimit)
			}
		}
		if cfg.Lint.BannedWords != nil {
			words := *cfg.Lint.BannedWords
			if len(words) > MaxBannedWordsCount {
				return nil, fmt.Errorf("%w: lint.banned_words exceeds %d entries", ErrInvalidArgument, MaxBannedWordsCount)
			}
			cleanWords := make([]string, 0, len(words))
			seen := make(map[string]bool, len(words))
			for _, w := range words {
				if strings.ContainsAny(w, "\r\n") {
					return nil, fmt.Errorf("%w: lint.banned_words entries must be single-line strings", ErrInvalidArgument)
				}
				clean := strings.TrimSpace(w)
				if clean == "" {
					continue
				}
				if utf8.RuneCountInString(clean) > MaxBannedWordRunes {
					return nil, fmt.Errorf("%w: banned word %q exceeds %d characters", ErrInvalidArgument, clean, MaxBannedWordRunes)
				}
				key := strings.ToLower(clean)
				if !seen[key] {
					seen[key] = true
					cleanWords = append(cleanWords, clean)
				}
			}
			cfg.Lint.BannedWords = &cleanWords
		}
	}

	if cfg.Timeline != nil && cfg.Timeline.PromptGuidance != nil {
		clean := strings.TrimSpace(*cfg.Timeline.PromptGuidance)
		if utf8.RuneCountInString(clean) > MaxTimelineGuidanceRunes {
			return nil, fmt.Errorf("%w: timeline.prompt_guidance exceeds %d characters", ErrInvalidArgument, MaxTimelineGuidanceRunes)
		}
		cfg.Timeline.PromptGuidance = &clean
	}

	if cfg.ExtraLLMsMarkdown != nil {
		clean := strings.TrimSpace(*cfg.ExtraLLMsMarkdown)
		if utf8.RuneCountInString(clean) > MaxExtraLLMsMarkdownRunes {
			return nil, fmt.Errorf("%w: extra_llms_markdown exceeds %d characters", ErrInvalidArgument, MaxExtraLLMsMarkdownRunes)
		}
		cfg.ExtraLLMsMarkdown = &clean
	}

	return &cfg, nil
}

// ResolveEffectiveBookConfig merges baseline CLI/env config with optional src/book.json bytes.
// If raw is non-empty and malformed, it fails safely back to baseline config and returns a warning.
func ResolveEffectiveBookConfig(base EngineConfig, raw []byte) (EffectiveBookConfig, string) {
	eff := defaultEffectiveBookConfig(base)
	if len(raw) == 0 {
		return eff, ""
	}
	parsed, err := ParseAndValidateBookConfig(raw)
	if err != nil {
		msg := strings.TrimSpace(strings.TrimPrefix(err.Error(), ErrInvalidArgument.Error()+":"))
		return eff, fmt.Sprintf("src/book.json could not be loaded (%s); falling back to server defaults.", msg)
	}
	eff.FromRepoFile = true
	if parsed.Title != nil && *parsed.Title != "" {
		eff.BookTitle = *parsed.Title
	}
	if parsed.Kicker != nil && *parsed.Kicker != "" {
		eff.BookKicker = *parsed.Kicker
	}
	if parsed.HidePoweredBy != nil {
		eff.HidePoweredBy = *parsed.HidePoweredBy
	}
	if parsed.Lint != nil {
		if parsed.Lint.MaxHeadingChars != nil {
			eff.Lint.MaxHeadingChars = *parsed.Lint.MaxHeadingChars
		}
		if parsed.Lint.ForbidEmDash != nil {
			eff.Lint.ForbidEmDash = *parsed.Lint.ForbidEmDash
		}
		if parsed.Lint.BannedWords != nil {
			eff.Lint.BannedWords = slices.Clone(*parsed.Lint.BannedWords)
		}
	}
	if parsed.Timeline != nil && parsed.Timeline.PromptGuidance != nil {
		eff.TimelineGuidance = *parsed.Timeline.PromptGuidance
	}
	if parsed.ExtraLLMsMarkdown != nil {
		eff.ExtraLLMsMarkdown = *parsed.ExtraLLMsMarkdown
	}
	return eff, ""
}

// FormatEffectiveBookConfigJSON serializes an EffectiveBookConfig into canonical 2-space JSON.
func FormatEffectiveBookConfigJSON(eff EffectiveBookConfig) string {
	banned := eff.Lint.BannedWords
	if banned == nil {
		banned = []string{}
	}
	payload := struct {
		Title         string `json:"title"`
		Kicker        string `json:"kicker"`
		HidePoweredBy bool   `json:"hide_powered_by"`
		Lint          struct {
			MaxHeadingChars int      `json:"max_heading_chars"`
			ForbidEmDash    bool     `json:"forbid_em_dash"`
			BannedWords     []string `json:"banned_words"`
		} `json:"lint"`
		Timeline struct {
			PromptGuidance string `json:"prompt_guidance"`
		} `json:"timeline"`
		ExtraLLMsMarkdown string `json:"extra_llms_markdown"`
	}{
		Title:             eff.BookTitle,
		Kicker:            eff.BookKicker,
		HidePoweredBy:     eff.HidePoweredBy,
		ExtraLLMsMarkdown: eff.ExtraLLMsMarkdown,
	}
	payload.Lint.MaxHeadingChars = eff.Lint.MaxHeadingChars
	payload.Lint.ForbidEmDash = eff.Lint.ForbidEmDash
	payload.Lint.BannedWords = banned
	payload.Timeline.PromptGuidance = eff.TimelineGuidance

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
	return buf.String()
}
