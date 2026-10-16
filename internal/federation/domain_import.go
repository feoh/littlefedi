package federation

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/models"
)

const (
	domainImportMaxBytes = 1 << 20
	domainImportMaxRows  = 10_000
)

func readDomainImport(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, domainImportMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > domainImportMaxBytes {
		return nil, fmt.Errorf("import exceeds 1 MiB")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("import is empty")
	}
	return body, nil
}

func ParseDomainAllowImport(r io.Reader) ([]*models.DomainAllow, error) {
	body, err := readDomainImport(r)
	if err != nil {
		return nil, err
	}
	first := strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0])
	var domains []string
	if first == "domain" || first == "#domain" {
		records, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		if err != nil {
			return nil, err
		}
		if len(records) > domainImportMaxRows+1 {
			return nil, fmt.Errorf("import exceeds %d rows", domainImportMaxRows)
		}
		for i, record := range records[1:] {
			if len(record) != 1 || strings.TrimSpace(record[0]) == "" {
				return nil, fmt.Errorf("invalid row %d", i+2)
			}
			domains = append(domains, record[0])
		}
	} else {
		scanner := bufio.NewScanner(bytes.NewReader(body))
		rows := 0
		for scanner.Scan() {
			rows++
			if rows > domainImportMaxRows {
				return nil, fmt.Errorf("import exceeds %d rows", domainImportMaxRows)
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			domains = append(domains, line)
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	if len(domains) == 0 {
		return nil, fmt.Errorf("import contains no domains")
	}
	seen := make(map[string]struct{}, len(domains))
	allows := make([]*models.DomainAllow, 0, len(domains))
	for _, domain := range domains {
		key := strings.ToLower(strings.TrimSpace(domain))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		allows = append(allows, &models.DomainAllow{Domain: domain})
	}
	return allows, nil
}

func ParseDomainBlockImport(r io.Reader) ([]*models.DomainBlock, error) {
	body, err := readDomainImport(r)
	if err != nil {
		return nil, err
	}
	first := strings.TrimSpace(strings.SplitN(string(body), "\n", 2)[0])
	if !strings.Contains(first, ",") {
		allows, err := ParseDomainAllowImport(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		blocks := make([]*models.DomainBlock, 0, len(allows))
		for _, allow := range allows {
			blocks = append(blocks, &models.DomainBlock{Domain: allow.Domain, Severity: "suspend"})
		}
		return blocks, nil
	}
	reader := csv.NewReader(bytes.NewReader(body))
	records, err := reader.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) < 2 {
		return nil, fmt.Errorf("import contains no domains")
	}
	if len(records) > domainImportMaxRows+1 {
		return nil, fmt.Errorf("import exceeds %d rows", domainImportMaxRows)
	}
	columns := make(map[string]int)
	for i, name := range records[0] {
		columns[strings.TrimPrefix(strings.TrimSpace(name), "#")] = i
	}
	if _, ok := columns["domain"]; !ok {
		return nil, fmt.Errorf("CSV requires a domain column")
	}
	value := func(record []string, name string) string {
		if i, ok := columns[name]; ok && i < len(record) {
			return strings.TrimSpace(record[i])
		}
		return ""
	}
	truthy := func(v string) bool { v = strings.ToLower(v); return v == "true" || v == "1" || v == "yes" }
	seen := make(map[string]struct{})
	blocks := make([]*models.DomainBlock, 0, len(records)-1)
	for i, record := range records[1:] {
		domain := value(record, "domain")
		if domain == "" {
			return nil, fmt.Errorf("invalid row %d", i+2)
		}
		key := strings.ToLower(domain)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		severity := value(record, "severity")
		if severity == "" {
			severity = "suspend"
		}
		blocks = append(blocks, &models.DomainBlock{Domain: domain, Severity: severity,
			Comment: value(record, "public_comment"), RejectMedia: truthy(value(record, "reject_media")),
			RejectReports: truthy(value(record, "reject_reports")), Obfuscate: truthy(value(record, "obfuscate"))})
	}
	return blocks, nil
}
