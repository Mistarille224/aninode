package main

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var (
	placeholderRE = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)
	quotedRE      = regexp.MustCompile(`"([^"\n]+)"`)
	templateRE    = regexp.MustCompile("`([^`]*)`")
	markupRE      = regexp.MustCompile(`>([^<>]+)<|(?:placeholder|aria-label|title)=["']([^"']+)["']`)
	latinRE       = regexp.MustCompile(`[A-Za-z]`)
)

var technicalText = set("aninode", ".aninode.json", "RSS", "— E", "https://…")
var brands = []string{"Emby", "Plex", "aninode", "qBittorrent", "Transmission", "aria2"}

type catalog struct {
	Schema   string                       `json:"$schema"`
	Locale   string                       `json:"locale"`
	Messages map[string]map[string]string `json:"messages"`
}

func main() {
	root, err := repositoryRoot()
	if err != nil {
		fatal(err)
	}
	if err := check(root); err != nil {
		fatal(err)
	}
}

func repositoryRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("cannot find repository root")
		}
		dir = parent
	}
}

func check(root string) error {
	web := filepath.Join(root, "internal", "server", "web")
	content, err := os.ReadFile(filepath.Join(web, "locales", "zh-CN.json"))
	if err != nil {
		return err
	}
	var pack catalog
	if err := json.Unmarshal(content, &pack); err != nil {
		return err
	}
	if pack.Schema != "aninode-i18n-v1" || pack.Locale != "zh-CN" || len(pack.Messages) == 0 {
		return fmt.Errorf("unexpected locale schema, locale, or message groups")
	}

	messages := make(map[string]string)
	var problems []string
	for groupName, group := range pack.Messages {
		if group == nil {
			problems = append(problems, "messages must contain named groups: "+groupName)
			continue
		}
		for source, translated := range group {
			if _, exists := messages[source]; exists {
				problems = append(problems, "Duplicate key in "+groupName+": "+source)
			}
			messages[source] = translated
			if source == "" || source != strings.TrimSpace(source) {
				problems = append(problems, fmt.Sprintf("Fragment or padded key in %s: %q", groupName, source))
			}
			if translated == "" || translated != strings.TrimSpace(translated) {
				problems = append(problems, fmt.Sprintf("Empty or padded translation: %q", source))
			}
			if !sameSet(placeholders(source), placeholders(translated)) {
				problems = append(problems, fmt.Sprintf("Placeholder mismatch: %q", source))
			}
			for _, brand := range brands {
				if strings.Contains(source, brand) && !strings.Contains(translated, brand) {
					problems = append(problems, fmt.Sprintf("Brand changed in translation: %q", source))
				}
			}
		}
	}

	var sources []string
	for _, name := range []string{"app.js", "controls.js", "index.html"} {
		body, err := os.ReadFile(filepath.Join(web, name))
		if err != nil {
			return err
		}
		sources = append(sources, string(body))
	}
	sourceText := strings.Join(sources, "\n")
	for _, call := range []string{"formatMessage", "translate", "button", "field", "toast", "modalError"} {
		pattern := regexp.MustCompile(`(?:^|[^A-Za-z0-9_])` + regexp.QuoteMeta(call) + `\(["']([^"']+)["']`)
		for _, match := range pattern.FindAllStringSubmatch(sourceText, -1) {
			phrase := match[1]
			if _, exists := messages[phrase]; !exists && !technicalText[phrase] {
				problems = append(problems, fmt.Sprintf("Missing %s translation: %q", call, phrase))
			}
		}
	}

	// Fixed API errors are shown directly in the Web UI.
	for _, name := range []string{"auth.go", "server.go"} {
		body, err := os.ReadFile(filepath.Join(root, "internal", "server", name))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "writeAPIError(") {
				continue
			}
			literals := quotedRE.FindAllStringSubmatch(line, -1)
			if len(literals) >= 2 {
				message := literals[len(literals)-1][1]
				if _, exists := messages[message]; !exists {
					problems = append(problems, fmt.Sprintf("Missing API error translation: %q", message))
				}
			}
		}
	}
	passwordError := "The sign-in password must be at least 8 characters and at most 1024 bytes"
	if _, exists := messages[passwordError]; !exists {
		problems = append(problems, fmt.Sprintf("Missing password validation translation: %q", passwordError))
	}

	// Static HTML text and accessible attributes are translated by localizeDOM.
	for _, template := range templateRE.FindAllStringSubmatch(sourceText, -1) {
		for _, match := range markupRE.FindAllStringSubmatch(template[1], -1) {
			phrase := match[1]
			if phrase == "" {
				phrase = match[2]
			}
			phrase = strings.TrimSpace(html.UnescapeString(phrase))
			_, translated := messages[phrase]
			if phrase != "" && !strings.Contains(phrase, "${") && latinRE.MatchString(phrase) &&
				!translated && !technicalText[phrase] && !strings.HasPrefix(phrase, "structuredRow(") {
				problems = append(problems, fmt.Sprintf("Missing static markup translation: %q", phrase))
			}
		}
	}

	if len(problems) != 0 {
		problems = uniqueSorted(problems)
		return fmt.Errorf("%s", strings.Join(problems, "\n"))
	}
	fmt.Printf("zh-CN: %d complete messages in %d sections; coverage checks passed\n", len(messages), len(pack.Messages))
	return nil
}

func placeholders(value string) map[string]bool {
	result := make(map[string]bool)
	for _, match := range placeholderRE.FindAllStringSubmatch(value, -1) {
		result[match[1]] = true
	}
	return result
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key := range a {
		if !b[key] {
			return false
		}
	}
	return true
}

func set(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	var result []string
	for _, value := range values {
		if !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	sort.Strings(result)
	return result
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
