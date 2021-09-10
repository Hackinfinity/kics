package secrets

import (
	"context"
	_ "embed" // Embed KICS regex rules
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/Checkmarx/kics/assets"
	"github.com/Checkmarx/kics/pkg/detector"
	"github.com/Checkmarx/kics/pkg/detector/docker"
	"github.com/Checkmarx/kics/pkg/detector/helm"
	engine "github.com/Checkmarx/kics/pkg/engine"
	"github.com/Checkmarx/kics/pkg/engine/similarity"
	"github.com/Checkmarx/kics/pkg/engine/source"
	"github.com/Checkmarx/kics/pkg/model"
	"github.com/rs/zerolog/log"
)

const (
	Base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
	HexChars    = "1234567890abcdefABCDEF"
)

var (
	SecretsQueryMetadata map[string]string
)

type Inspector struct {
	ctx                   context.Context
	tracker               engine.Tracker
	detector              *detector.DetectLine
	excludeResults        map[string]bool
	regexQueries          []RegexQuery
	vulnerabilities       []model.Vulnerability
	queryExecutionTimeout time.Duration
}

type Entropy struct {
	Group int     `json:"group"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
}

type MultilineResult struct {
	DetectLineGroup int `json:"detectLineGroup"`
}

type AllowRule struct {
	Description string `json:"description"`
	RegexStr    string `json:"regex"`
	Regex       *regexp.Regexp
}

type RegexQuery struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Multiline  MultilineResult `json:"multiline"`
	RegexStr   string          `json:"regex"`
	Entropies  []Entropy       `json:"entropies"`
	AllowRules []AllowRule     `json:"allowRules"`
	Regex      *regexp.Regexp
}

type RuleMatch struct {
	File     string
	RuleName string
	Matches  []string
	Line     int
	Entropy  float64
}

func NewInspector(
	ctx context.Context,
	excludeResults map[string]bool,
	tracker engine.Tracker,
	queryFilter *source.QueryInspectorParameters,
	disableSecretsQuery bool,
	executionTimeout int,
	regexRulesContent string,
) (*Inspector, error) {
	if disableSecretsQuery {
		return &Inspector{
			ctx:                   ctx,
			tracker:               tracker,
			excludeResults:        excludeResults,
			regexQueries:          make([]RegexQuery, 0),
			vulnerabilities:       make([]model.Vulnerability, 0),
			queryExecutionTimeout: time.Duration(executionTimeout) * time.Second,
		}, nil
	}

	lineDetector := detector.NewDetectLine(tracker.GetOutputLines()).
		Add(helm.DetectKindLine{}, model.KindHELM).
		Add(docker.DetectKindLine{}, model.KindDOCKER)

	err := json.Unmarshal([]byte(assets.SecretsQueryMetadataJSON), &SecretsQueryMetadata)
	if err != nil {
		return nil, err
	}
	queryExecutionTimeout := time.Duration(executionTimeout) * time.Second

	var allRegexQueries []RegexQuery
	err = json.Unmarshal([]byte(regexRulesContent), &allRegexQueries)
	if err != nil {
		return nil, err
	}

	return &Inspector{
		ctx:                   ctx,
		detector:              lineDetector,
		excludeResults:        excludeResults,
		tracker:               tracker,
		regexQueries:          compileRegexQueries(queryFilter, allRegexQueries),
		vulnerabilities:       make([]model.Vulnerability, 0),
		queryExecutionTimeout: queryExecutionTimeout,
	}, nil
}

func (c *Inspector) Inspect(ctx context.Context, basePaths []string,
	files model.FileMetadatas, currentQuery chan<- int64) ([]model.Vulnerability, error) {
	for i := range c.regexQueries {
		currentQuery <- 1

		timeoutCtx, cancel := context.WithTimeout(ctx, c.queryExecutionTimeout*time.Second)
		defer cancel()
		for idx := range files {
			select {
			case <-timeoutCtx.Done():
				return c.vulnerabilities, timeoutCtx.Err()
			default:
				// check file content line by line
				if c.regexQueries[i].Multiline == (MultilineResult{}) {
					lines := c.detector.SplitLines(&files[idx])

					for lineNumber, currentLine := range lines {
						c.checkLineByLine(&c.regexQueries[i], basePaths, &files[idx], lineNumber, currentLine)
					}
					continue
				}

				// check file content as a whole
				c.checkFileContent(&c.regexQueries[i], basePaths, &files[idx])
			}
		}
	}
	return c.vulnerabilities, nil
}

func compileRegexQueries(queryFilter *source.QueryInspectorParameters, allRegexQueries []RegexQuery) []RegexQuery {
	var regexQueries []RegexQuery

	for i := range allRegexQueries {
		if len(queryFilter.IncludeQueries.ByIDs) > 0 {
			if isValueInArray(allRegexQueries[i].ID, queryFilter.IncludeQueries.ByIDs) {
				regexQueries = append(regexQueries, allRegexQueries[i])
			}
		} else {
			if isValueInArray(allRegexQueries[i].ID, queryFilter.ExcludeQueries.ByIDs) {
				log.Debug().
					Msgf("Excluding query ID: %s category: %s severity: %s",
						allRegexQueries[i].ID,
						SecretsQueryMetadata["category"],
						SecretsQueryMetadata["severity"])
				continue
			}
			regexQueries = append(regexQueries, allRegexQueries[i])
		}
	}
	for i := range regexQueries {
		regexQueries[i].Regex = regexp.MustCompile(regexQueries[i].RegexStr)
		for j := range regexQueries[i].AllowRules {
			regexQueries[i].AllowRules[j].Regex = regexp.MustCompile(regexQueries[i].AllowRules[j].RegexStr)
		}
	}
	return regexQueries
}

func (c *Inspector) GetQueriesLength() int {
	return len(c.regexQueries)
}

func isValueInArray(value string, array []string) bool {
	for i := range array {
		if value == array[i] {
			return true
		}
	}
	return false
}

func isSecret(s string, query *RegexQuery) (isSecret bool, groups []string) {
	if isAllowRule(s, query.AllowRules) {
		return false, []string{}
	}

	groups = query.Regex.FindStringSubmatch(s)
	if len(groups) > 0 {
		return true, groups
	}
	return false, []string{}
}

func isAllowRule(s string, allowRules []AllowRule) bool {
	for i := range allowRules {
		if allowRules[i].Regex.MatchString(s) {
			return true
		}
	}
	return false
}

func (c *Inspector) checkFileContent(query *RegexQuery, basePaths []string, file *model.FileMetadata) {
	isSecret, groups := isSecret(file.OriginalData, query)
	if !isSecret {
		return
	}

	lineContent, lineNumber := c.secretsDetectLine(query, file, groups)

	if len(query.Entropies) == 0 {
		c.addVulnerability(
			basePaths,
			file,
			query,
			lineNumber,
			lineContent,
		)
	}

	if len(groups) > 0 {
		for _, entropy := range query.Entropies {
			// if matched group does not exist continue
			if len(groups) <= entropy.Group {
				return
			}
			isMatch, entropyFloat := CheckEntropyInterval(
				entropy,
				groups[entropy.Group],
			)
			log.Debug().Msgf("match: %v :: %v", isMatch, fmt.Sprint(entropyFloat))

			if isMatch {
				c.addVulnerability(
					basePaths,
					file,
					query,
					lineNumber,
					lineContent,
				)
			}
		}
	}
}

func (c *Inspector) secretsDetectLine(query *RegexQuery, file *model.FileMetadata, groups []string) (lineContent string, lineNumber int) {
	lineNumber = -1
	lineContent = "-"

	if len(groups) <= query.Multiline.DetectLineGroup {
		log.Warn().Msgf("Unable to detect line in file %v Multiline group not found: %v", file.FilePath, query.Multiline.DetectLineGroup)
		return lineContent, lineNumber
	}

	lines := c.detector.SplitLines(file)
	contentMatchRemoved := strings.ReplaceAll(file.OriginalData, groups[query.Multiline.DetectLineGroup], "")

	text := strings.ReplaceAll(contentMatchRemoved, "\r", "")
	contentMatchRemovedLines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if lines[i] != contentMatchRemovedLines[i] {
			lineNumber = i
			lineContent = lines[i]
			break
		}
	}

	return lineContent, lineNumber
}

func (c *Inspector) checkLineByLine(query *RegexQuery, basePaths []string, file *model.FileMetadata, lineNumber int, currentLine string) {
	isSecret, groups := isSecret(currentLine, query)
	if !isSecret {
		return
	}

	if len(query.Entropies) == 0 {
		c.addVulnerability(
			basePaths,
			file,
			query,
			lineNumber,
			currentLine,
		)
	}

	for i := range query.Entropies {
		entropy := query.Entropies[i]

		// if matched group does not exist continue
		if len(groups) <= entropy.Group {
			return
		}

		isMatch, entropyFloat := CheckEntropyInterval(
			entropy,
			groups[entropy.Group],
		)
		log.Debug().Msgf("match: %v :: %v", isMatch, fmt.Sprint(entropyFloat))

		if isMatch {
			c.addVulnerability(
				basePaths,
				file,
				query,
				lineNumber,
				currentLine,
			)
		}
	}
}

func (c *Inspector) addVulnerability(basePaths []string, file *model.FileMetadata, query *RegexQuery, lineNumber int, issueLine string) {
	simID, err := similarity.ComputeSimilarityID(
		basePaths,
		file.FilePath,
		query.ID,
		fmt.Sprintf("%d", lineNumber),
		"",
	)
	if err != nil {
		log.Error().Msg("unable to compute similarity ID")
	}

	if _, ok := c.excludeResults[engine.PtrStringToString(simID)]; !ok {
		linesVuln := model.VulnerabilityLines{
			Line:      -1,
			VulnLines: []model.CodeLine{},
		}
		linesVuln = c.detector.GetAdjecent(file, lineNumber+1)
		vuln := model.Vulnerability{
			QueryID:          query.ID,
			QueryName:        SecretsQueryMetadata["queryName"] + " - " + query.Name,
			SimilarityID:     engine.PtrStringToString(simID),
			FileID:           file.ID,
			FileName:         file.FilePath,
			Line:             linesVuln.Line,
			VulnLines:        linesVuln.VulnLines,
			IssueType:        "RedundantAttribute",
			Platform:         SecretsQueryMetadata["platform"],
			Severity:         model.SeverityHigh,
			QueryURI:         SecretsQueryMetadata["descriptionUrl"],
			Category:         SecretsQueryMetadata["category"],
			Description:      SecretsQueryMetadata["descriptionText"],
			DescriptionID:    SecretsQueryMetadata["descriptionID"],
			KeyExpectedValue: "Hardcoded secret key should not appear in source",
			KeyActualValue:   fmt.Sprintf("'%s' contains a secret", issueLine),
		}
		c.vulnerabilities = append(c.vulnerabilities, vuln)
	}
}

// CheckEntropyInterval - verifies if a given token's entropy is within expected bounds
func CheckEntropyInterval(entropy Entropy, token string) (isEntropyInInterval bool, entropyLevel float64) {
	base64Entropy := calculateEntropy(token, Base64Chars)
	hexEntropy := calculateEntropy(token, HexChars)
	highestEntropy := math.Max(base64Entropy, hexEntropy)
	if insideInterval(entropy, base64Entropy) || insideInterval(entropy, hexEntropy) {
		return true, highestEntropy
	}
	return false, highestEntropy
}

func insideInterval(entropy Entropy, floatEntropy float64) bool {
	return floatEntropy >= entropy.Min && floatEntropy <= entropy.Max
}

// calculateEntropy - calculates the entropy of a string based on the Shannon formula
func calculateEntropy(token, charSet string) float64 {
	if token == "" {
		return 0
	}
	charMap := map[rune]float64{}
	for _, char := range token {
		if strings.Contains(charSet, string(char)) {
			charMap[char]++
		}
	}

	var freq float64
	length := float64(len(token))
	for _, count := range charMap {
		freq += count * math.Log2(count)
	}

	return math.Log2(length) - freq/length
}