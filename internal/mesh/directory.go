package mesh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"brew.bsd.cafe/littlecomputerpeople/littlefedi/internal/netutil"
)

const (
	DirectoryRecordVersion   = 1
	MaxDirectoryRecordBytes  = 4 << 10
	MaxDirectoryLighthouses  = 3
	MaxDirectoryRecordTTL    = 7 * 24 * time.Hour
	directoryPublishInterval = time.Second
)

var errDirectoryPublishRateLimit = errors.New("mesh directory: publish rate limit")
var errDirectoryCapacity = errors.New("mesh directory: record capacity reached")

type LighthouseLocator struct {
	Address string `json:"address"`
	Pin     string `json:"pin"`
}

// DirectoryLocator converts one trusted node configuration entry into the
// stricter public, pinned form accepted by directory records.
func DirectoryLocator(entry string) (LighthouseLocator, error) {
	address, pin, found := strings.Cut(strings.TrimSpace(entry), "@")
	locator := LighthouseLocator{
		Address: strings.TrimSpace(address),
		Pin:     strings.ToLower(strings.TrimSpace(pin)),
	}
	if !found {
		return LighthouseLocator{}, errors.New("mesh directory: lighthouse entry has no identity pin")
	}
	if err := validateLighthouseLocator(locator); err != nil {
		return LighthouseLocator{}, err
	}
	return locator, nil
}

// DirectoryPayload is signed exactly as its JSON bytes appear in
// SignedDirectoryRecord.Payload. The node ID remains the transport identity;
// Alias and Lighthouses are replaceable naming and reachability metadata.
type DirectoryPayload struct {
	Version     int                 `json:"version"`
	NodeID      string              `json:"node_id"`
	Alias       string              `json:"alias"`
	Sequence    uint64              `json:"sequence"`
	ExpiresAt   int64               `json:"expires_at"`
	Lighthouses []LighthouseLocator `json:"lighthouses"`
}

type SignedDirectoryRecord struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

type DirectoryHTTPError struct {
	StatusCode int
}

func (e *DirectoryHTTPError) Error() string {
	return fmt.Sprintf("mesh directory: request returned %d", e.StatusCode)
}

func SignDirectoryRecord(id *Identity, payload DirectoryPayload) (SignedDirectoryRecord, error) {
	if id == nil {
		return SignedDirectoryRecord{}, errors.New("mesh directory: identity is required")
	}
	payload.NodeID = strings.ToLower(payload.NodeID)
	payload.Alias = normalizeDirectoryName(payload.Alias)
	for i := range payload.Lighthouses {
		payload.Lighthouses[i].Pin = strings.ToLower(payload.Lighthouses[i].Pin)
	}
	if payload.Lighthouses == nil {
		payload.Lighthouses = []LighthouseLocator{}
	}
	if payload.NodeID != id.NodeID {
		return SignedDirectoryRecord{}, errors.New("mesh directory: record node ID does not match signer")
	}
	if err := validateDirectoryPayload(payload, time.Now()); err != nil {
		return SignedDirectoryRecord{}, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return SignedDirectoryRecord{}, fmt.Errorf("mesh directory: encode payload: %w", err)
	}
	record := SignedDirectoryRecord{
		Payload:   raw,
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(id.priv, raw)),
	}
	if encoded, _ := json.Marshal(record); len(encoded) > MaxDirectoryRecordBytes {
		return SignedDirectoryRecord{}, fmt.Errorf("mesh directory: record exceeds %d bytes", MaxDirectoryRecordBytes)
	}
	return record, nil
}

func VerifyDirectoryRecord(record SignedDirectoryRecord, now time.Time) (DirectoryPayload, error) {
	if len(record.Payload) == 0 {
		return DirectoryPayload{}, errors.New("mesh directory: missing payload")
	}
	var payload DirectoryPayload
	decoder := json.NewDecoder(bytes.NewReader(record.Payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return DirectoryPayload{}, fmt.Errorf("mesh directory: decode payload: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DirectoryPayload{}, err
	}
	if err := validateDirectoryPayload(payload, now); err != nil {
		return DirectoryPayload{}, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(record.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return DirectoryPayload{}, errors.New("mesh directory: invalid signature encoding")
	}
	publicKey, err := PublicKeyFromNodeID(payload.NodeID)
	if err != nil || !ed25519.Verify(publicKey, record.Payload, signature) {
		return DirectoryPayload{}, errors.New("mesh directory: signature verification failed")
	}
	return payload, nil
}

func validateDirectoryPayload(payload DirectoryPayload, now time.Time) error {
	if payload.Version != DirectoryRecordVersion {
		return fmt.Errorf("mesh directory: unsupported record version %d", payload.Version)
	}
	if payload.NodeID != strings.ToLower(payload.NodeID) || !ValidNodeID(payload.NodeID) {
		return errors.New("mesh directory: invalid node ID")
	}
	if payload.Alias != normalizeDirectoryName(payload.Alias) || !validDirectoryName(payload.Alias) {
		return errors.New("mesh directory: invalid alias")
	}
	if payload.Sequence == 0 {
		return errors.New("mesh directory: sequence must be positive")
	}
	if payload.ExpiresAt <= now.Unix() {
		return errors.New("mesh directory: record is expired")
	}
	if !now.Equal(time.Unix(0, 0)) && payload.ExpiresAt > now.Add(MaxDirectoryRecordTTL).Unix() {
		return fmt.Errorf("mesh directory: record lifetime exceeds %s", MaxDirectoryRecordTTL)
	}
	if payload.Lighthouses == nil {
		return errors.New("mesh directory: missing lighthouse list")
	}
	if len(payload.Lighthouses) > MaxDirectoryLighthouses {
		return fmt.Errorf("mesh directory: at most %d lighthouses are allowed", MaxDirectoryLighthouses)
	}
	for _, locator := range payload.Lighthouses {
		if err := validateLighthouseLocator(locator); err != nil {
			return err
		}
	}
	return nil
}

func validateLighthouseLocator(locator LighthouseLocator) error {
	if locator.Pin == "" || locator.Pin != strings.ToLower(locator.Pin) || !ValidNodeID(locator.Pin) {
		return errors.New("mesh directory: every lighthouse needs a valid Ed25519 pin")
	}
	address := strings.TrimSpace(locator.Address)
	if address == "" || address != locator.Address || strings.ContainsAny(address, "/?#@") {
		return fmt.Errorf("mesh directory: invalid lighthouse address %q", locator.Address)
	}
	host, port, err := net.SplitHostPort(withDefaultPort(address))
	if err != nil {
		return fmt.Errorf("mesh directory: invalid lighthouse address %q", address)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("mesh directory: invalid lighthouse port %q", port)
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if ip := net.ParseIP(host); ip != nil {
		if !netutil.IsPublicIP(ip) {
			return fmt.Errorf("mesh directory: lighthouse %q is not public", host)
		}
		return nil
	}
	if !validDirectoryName(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("mesh directory: invalid lighthouse host %q", host)
	}
	return nil
}

func normalizeDirectoryName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

func validDirectoryName(name string) bool {
	if name == "" || len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("mesh directory: trailing JSON data")
	}
	return nil
}

// Directory stores signed alias and lighthouse records. NewDirectory is
// useful for tests; NewPersistentDirectory adds SQLite persistence.
type Directory struct {
	writeMu           sync.Mutex
	mu                sync.RWMutex
	byNode            map[string]SignedDirectoryRecord
	byAlias           map[string]string
	lastPublish       map[string]time.Time
	reservations      map[string]string
	reservationByNode map[string]string
	db                *sql.DB
	openRegistration  bool
	maxRecords        int
}

func NewDirectory() *Directory {
	return &Directory{
		byNode:            make(map[string]SignedDirectoryRecord),
		byAlias:           make(map[string]string),
		lastPublish:       make(map[string]time.Time),
		reservations:      make(map[string]string),
		reservationByNode: make(map[string]string),
		openRegistration:  true,
	}
}

// NewPersistentDirectory opens the first-release SQLite registry. Closed
// registration is the safe default: reserve an alias for its node ID before
// the first publication, or explicitly enable openRegistration.
func NewPersistentDirectory(path string, openRegistration bool, maxRecords int) (*Directory, error) {
	directory := NewDirectory()
	directory.openRegistration = openRegistration
	directory.maxRecords = maxRecords
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("mesh directory: state path is required")
	}
	db, err := openDirectorySQLite(path)
	if err != nil {
		return nil, err
	}
	directory.db = db
	if err := initializeDirectoryDB(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := loadDirectoryRecords(db, directory); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := loadDirectoryReservations(db, directory); err != nil {
		_ = db.Close()
		return nil, err
	}
	return directory, nil
}

func loadDirectoryRecords(db *sql.DB, directory *Directory) error {
	rows, err := db.Query(`SELECT node_id, alias, payload, signature, last_publish_ns FROM directory_records`)
	if err != nil {
		return fmt.Errorf("mesh directory: read records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID, alias, signature string
		var raw []byte
		var lastPublish int64
		if err := rows.Scan(&nodeID, &alias, &raw, &signature, &lastPublish); err != nil {
			return fmt.Errorf("mesh directory: read record: %w", err)
		}
		record := SignedDirectoryRecord{Payload: append(json.RawMessage(nil), raw...), Signature: signature}
		payload, err := VerifyDirectoryRecord(record, time.Unix(0, 0))
		if err != nil {
			return fmt.Errorf("mesh directory: invalid stored record: %w", err)
		}
		if payload.NodeID != nodeID || payload.Alias != alias {
			return errors.New("mesh directory: stored record index does not match its signature")
		}
		directory.byNode[nodeID] = record
		directory.byAlias[alias] = nodeID
		if lastPublish != 0 {
			directory.lastPublish[nodeID] = time.Unix(0, lastPublish)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("mesh directory: read records: %w", err)
	}
	return nil
}

func loadDirectoryReservations(db *sql.DB, directory *Directory) error {
	reservations, err := db.Query(`SELECT alias, node_id FROM directory_reservations`)
	if err != nil {
		return fmt.Errorf("mesh directory: read reservations: %w", err)
	}
	defer reservations.Close()
	for reservations.Next() {
		var alias, nodeID string
		if err := reservations.Scan(&alias, &nodeID); err != nil {
			return fmt.Errorf("mesh directory: read reservation: %w", err)
		}
		if !validDirectoryName(alias) || !ValidNodeID(nodeID) {
			return errors.New("mesh directory: invalid stored reservation")
		}
		directory.reservations[alias] = nodeID
		directory.reservationByNode[nodeID] = alias
	}
	if err := reservations.Err(); err != nil {
		return fmt.Errorf("mesh directory: read reservations: %w", err)
	}
	return nil
}

func (d *Directory) refreshReservation(alias string) error {
	if d.db == nil {
		return nil
	}
	var nodeID string
	err := d.db.QueryRow(
		`SELECT node_id FROM directory_reservations WHERE alias = ?`, alias,
	).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("mesh directory: refresh reservation: %w", err)
	}
	if !ValidNodeID(nodeID) {
		return errors.New("mesh directory: invalid stored reservation")
	}
	d.mu.Lock()
	d.reservations[alias] = nodeID
	d.reservationByNode[nodeID] = alias
	d.mu.Unlock()
	return nil
}

func initializeDirectoryDB(db *sql.DB) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS directory_records (
			node_id TEXT PRIMARY KEY,
			alias TEXT NOT NULL UNIQUE,
			payload BLOB NOT NULL,
			signature TEXT NOT NULL,
			last_publish_ns INTEGER NOT NULL DEFAULT 0
		) STRICT`,
		`CREATE TABLE IF NOT EXISTS directory_reservations (
			alias TEXT PRIMARY KEY,
			node_id TEXT NOT NULL UNIQUE
		) STRICT`,
	} {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("mesh directory: initialize database: %w", err)
		}
	}
	return nil
}

// Reserve permanently binds one human alias to one node ID. It is idempotent
// for the same pair and has no automatic reassignment operation.
func (d *Directory) Reserve(alias, nodeID string) error {
	alias = normalizeDirectoryName(alias)
	nodeID = strings.ToLower(strings.TrimSpace(nodeID))
	if !validDirectoryName(alias) || !ValidNodeID(nodeID) {
		return errors.New("mesh directory: invalid alias reservation")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	d.mu.RLock()
	aliasOwner, aliasExists := d.reservations[alias]
	nodeAlias, nodeExists := d.reservationByNode[nodeID]
	assignedOwner, aliasAssigned := d.byAlias[alias]
	assignedRecord, nodeAssigned := d.byNode[nodeID]
	d.mu.RUnlock()
	if aliasExists && aliasOwner != nodeID {
		return errors.New("mesh directory: alias already reserved")
	}
	if nodeExists && nodeAlias != alias {
		return errors.New("mesh directory: node already has a reserved alias")
	}
	if aliasAssigned && assignedOwner != nodeID {
		return errors.New("mesh directory: alias already assigned")
	}
	if nodeAssigned {
		payload, err := VerifyDirectoryRecord(assignedRecord, time.Unix(0, 0))
		if err != nil || payload.Alias != alias {
			return errors.New("mesh directory: node already has an assigned alias")
		}
	}
	if d.db != nil {
		if _, err := d.db.Exec(
			`INSERT INTO directory_reservations(alias, node_id) VALUES (?, ?)
			 ON CONFLICT(alias) DO NOTHING`, alias, nodeID,
		); err != nil {
			return fmt.Errorf("mesh directory: reserve alias: %w", err)
		}
		var storedNode string
		if err := d.db.QueryRow(
			`SELECT node_id FROM directory_reservations WHERE alias = ?`, alias,
		).Scan(&storedNode); err != nil || storedNode != nodeID {
			return errors.New("mesh directory: alias already reserved")
		}
	}
	d.mu.Lock()
	d.reservations[alias] = nodeID
	d.reservationByNode[nodeID] = alias
	d.mu.Unlock()
	return nil
}

func (d *Directory) Close() error {
	if d.db == nil {
		return nil
	}
	return d.db.Close()
}

func (d *Directory) Publish(record SignedDirectoryRecord, now time.Time) error {
	payload, err := VerifyDirectoryRecord(record, now)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > MaxDirectoryRecordBytes {
		return fmt.Errorf("mesh directory: record exceeds %d bytes", MaxDirectoryRecordBytes)
	}
	return d.publishVerified(record, payload, now, false)
}

func (d *Directory) publishVerified(record SignedDirectoryRecord, payload DirectoryPayload, now time.Time, rateLimit bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	if !d.openRegistration {
		d.mu.RLock()
		_, existingNode := d.byNode[payload.NodeID]
		reserved := d.reservations[payload.Alias] == payload.NodeID
		d.mu.RUnlock()
		if !existingNode && !reserved {
			if err := d.refreshReservation(payload.Alias); err != nil {
				return &directoryStorageError{err}
			}
		}
	}
	d.mu.RLock()
	if old, ok := d.byNode[payload.NodeID]; ok {
		oldPayload, err := VerifyDirectoryRecord(old, time.Unix(0, 0))
		if err != nil {
			d.mu.RUnlock()
			return errors.New("mesh directory: stored record is invalid")
		}
		if payload.Sequence <= oldPayload.Sequence {
			d.mu.RUnlock()
			return errors.New("mesh directory: sequence replay or rollback")
		}
		if payload.Alias != oldPayload.Alias {
			d.mu.RUnlock()
			return errors.New("mesh directory: alias is immutable")
		}
	}
	if owner, exists := d.byAlias[payload.Alias]; exists && owner != payload.NodeID {
		d.mu.RUnlock()
		return errors.New("mesh directory: alias already assigned")
	}
	if rateLimit && now.Sub(d.lastPublish[payload.NodeID]) < directoryPublishInterval {
		d.mu.RUnlock()
		return errDirectoryPublishRateLimit
	}
	_, existingNode := d.byNode[payload.NodeID]
	if !existingNode {
		if d.maxRecords > 0 && len(d.byNode) >= d.maxRecords {
			d.mu.RUnlock()
			return errDirectoryCapacity
		}
		if !d.openRegistration && d.reservations[payload.Alias] != payload.NodeID {
			d.mu.RUnlock()
			return errors.New("mesh directory: alias is not reserved for this node")
		}
	}
	published := d.lastPublish[payload.NodeID]
	if rateLimit {
		published = now
	}
	d.mu.RUnlock()
	if err := d.persistRecord(record, payload, published); err != nil {
		return &directoryStorageError{err}
	}
	d.mu.Lock()
	d.byNode[payload.NodeID] = cloneDirectoryRecord(record)
	d.byAlias[payload.Alias] = payload.NodeID
	if rateLimit {
		d.lastPublish[payload.NodeID] = now
	}
	d.mu.Unlock()
	return nil
}

type directoryStorageError struct {
	err error
}

func (e *directoryStorageError) Error() string { return e.err.Error() }
func (e *directoryStorageError) Unwrap() error { return e.err }

func (d *Directory) persistRecord(record SignedDirectoryRecord, payload DirectoryPayload, published time.Time) error {
	if d.db == nil {
		return nil
	}
	publishedAt := int64(0)
	if !published.IsZero() {
		publishedAt = published.UnixNano()
	}
	result, err := d.db.Exec(
		`INSERT INTO directory_records(node_id, alias, payload, signature, last_publish_ns)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
			payload = excluded.payload,
			signature = excluded.signature,
			last_publish_ns = excluded.last_publish_ns
		 WHERE directory_records.alias = excluded.alias`,
		payload.NodeID, payload.Alias, []byte(record.Payload), record.Signature, publishedAt,
	)
	if err != nil {
		return fmt.Errorf("mesh directory: persist record: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mesh directory: persist record: %w", err)
	}
	if affected != 1 {
		return errors.New("mesh directory: persistent alias conflict")
	}
	return nil
}

func (d *Directory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/records/"):
		d.put(w, r, strings.TrimPrefix(r.URL.Path, "/v1/records/"))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/nodes/"):
		d.getNode(w, strings.TrimPrefix(r.URL.Path, "/v1/nodes/"))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/aliases/"):
		alias, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/v1/aliases/"))
		if err != nil {
			http.Error(w, "invalid alias", http.StatusBadRequest)
			return
		}
		d.getAlias(w, alias)
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		if d.db != nil {
			if err := d.db.PingContext(r.Context()); err != nil {
				http.Error(w, "database unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		d.mu.RLock()
		w.Header().Set("X-LittleMesh-Records", strconv.Itoa(len(d.byNode)))
		w.Header().Set("X-LittleMesh-Reservations", strconv.Itoa(len(d.reservations)))
		if d.maxRecords > 0 {
			w.Header().Set("X-LittleMesh-Max-Records", strconv.Itoa(d.maxRecords))
		}
		d.mu.RUnlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (d *Directory) put(w http.ResponseWriter, r *http.Request, nodeID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxDirectoryRecordBytes+1))
	if err != nil || len(body) > MaxDirectoryRecordBytes {
		http.Error(w, "record too large", http.StatusRequestEntityTooLarge)
		return
	}
	var record SignedDirectoryRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil || ensureJSONEOF(decoder) != nil {
		http.Error(w, "invalid record", http.StatusBadRequest)
		return
	}
	payload, err := VerifyDirectoryRecord(record, time.Now())
	if err != nil || payload.NodeID != strings.ToLower(nodeID) {
		http.Error(w, "invalid record", http.StatusBadRequest)
		return
	}

	if err := d.publishVerified(record, payload, time.Now(), true); err != nil {
		status := http.StatusConflict
		var storageErr *directoryStorageError
		if errors.Is(err, errDirectoryPublishRateLimit) {
			status = http.StatusTooManyRequests
		} else if errors.Is(err, errDirectoryCapacity) {
			status = http.StatusInsufficientStorage
		} else if errors.As(err, &storageErr) {
			status = http.StatusInternalServerError
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Directory) getNode(w http.ResponseWriter, nodeID string) {
	d.mu.RLock()
	record, ok := d.byNode[strings.ToLower(nodeID)]
	d.mu.RUnlock()
	if !ok {
		http.NotFound(w, nil)
		return
	}
	writeDirectoryRecord(w, record)
}

func (d *Directory) getAlias(w http.ResponseWriter, alias string) {
	d.mu.RLock()
	nodeID, ok := d.byAlias[normalizeDirectoryName(alias)]
	record := d.byNode[nodeID]
	d.mu.RUnlock()
	if !ok {
		http.NotFound(w, nil)
		return
	}
	writeDirectoryRecord(w, record)
}

func writeDirectoryRecord(w http.ResponseWriter, record SignedDirectoryRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	encoded, _ := json.Marshal(record)
	_, _ = w.Write(encoded)
}

func cloneDirectoryRecord(record SignedDirectoryRecord) SignedDirectoryRecord {
	return SignedDirectoryRecord{
		Payload:   append(json.RawMessage(nil), record.Payload...),
		Signature: record.Signature,
	}
}

// DirectoryClient verifies directory answers and caches them until expiry.
type DirectoryClient struct {
	baseURL string
	client  *http.Client
	db      *sql.DB

	mu       sync.Mutex
	byNode   map[string]DirectoryPayload
	byAlias  map[string]DirectoryPayload
	highest  map[string]uint64
	bindings map[string]string
}

type directoryClientState struct {
	ByNode   map[string]DirectoryPayload `json:"by_node"`
	Highest  map[string]uint64           `json:"highest"`
	Bindings map[string]string           `json:"bindings"`
}

func NewDirectoryClient(baseURL string, client *http.Client) *DirectoryClient {
	if client == nil {
		client = &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return &DirectoryClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		client:   client,
		byNode:   make(map[string]DirectoryPayload),
		byAlias:  make(map[string]DirectoryPayload),
		highest:  make(map[string]uint64),
		bindings: make(map[string]string),
	}
}

// NewPersistentDirectoryClient keeps verified cache entries, alias bindings,
// and sequence high-water marks across process restarts.
func NewPersistentDirectoryClient(baseURL string, client *http.Client, path string) (*DirectoryClient, error) {
	directory := NewDirectoryClient(baseURL, client)
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("mesh directory: client state path is required")
	}
	db, err := openDirectorySQLite(path)
	if err != nil {
		return nil, err
	}
	directory.db = db
	if err := initializeDirectoryClientDB(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	state, err := readDirectoryClientState(db, "", "")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := directory.mergeStateLocked(state); err != nil {
		_ = db.Close()
		return nil, err
	}
	return directory, nil
}

func initializeDirectoryClientDB(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS directory_client_state (
		node_id TEXT PRIMARY KEY,
		alias TEXT NOT NULL UNIQUE,
		payload BLOB NOT NULL,
		highest BLOB NOT NULL CHECK(length(highest) = 8)
	) STRICT`)
	if err != nil {
		return fmt.Errorf("mesh directory: initialize client database: %w", err)
	}
	return nil
}

func readDirectoryClientState(db *sql.DB, nodeID, alias string) (directoryClientState, error) {
	query := `SELECT node_id, alias, payload, highest FROM directory_client_state`
	var args []any
	if nodeID != "" || alias != "" {
		query += ` WHERE node_id = ? OR alias = ?`
		args = []any{nodeID, alias}
	}
	rows, err := db.Query(query, args...)
	if err != nil {
		return directoryClientState{}, fmt.Errorf("mesh directory: read client state: %w", err)
	}
	defer rows.Close()
	state := directoryClientState{
		ByNode: make(map[string]DirectoryPayload), Highest: make(map[string]uint64),
		Bindings: make(map[string]string),
	}
	for rows.Next() {
		var storedNode, storedAlias string
		var raw, highest []byte
		if err := rows.Scan(&storedNode, &storedAlias, &raw, &highest); err != nil {
			return directoryClientState{}, fmt.Errorf("mesh directory: read client state: %w", err)
		}
		var payload DirectoryPayload
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil || ensureJSONEOF(decoder) != nil {
			return directoryClientState{}, errors.New("mesh directory: invalid cached payload")
		}
		sequence, err := decodeDirectorySequence(highest)
		if err != nil {
			return directoryClientState{}, err
		}
		if payload.NodeID != storedNode || payload.Alias != storedAlias || payload.Sequence > sequence {
			return directoryClientState{}, errors.New("mesh directory: cached payload index is inconsistent")
		}
		state.ByNode[storedNode] = payload
		state.Highest[storedNode] = sequence
		state.Bindings[storedAlias] = storedNode
	}
	if err := rows.Err(); err != nil {
		return directoryClientState{}, fmt.Errorf("mesh directory: read client state: %w", err)
	}
	return state, nil
}

func encodeDirectorySequence(sequence uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, sequence)
	return encoded
}

func decodeDirectorySequence(encoded []byte) (uint64, error) {
	if len(encoded) != 8 {
		return 0, errors.New("mesh directory: invalid cached sequence")
	}
	return binary.BigEndian.Uint64(encoded), nil
}

func (c *DirectoryClient) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *DirectoryClient) mergeStateLocked(state directoryClientState) error {
	aliases := make(map[string]string, len(state.ByNode))
	nodes := make(map[string]string, len(c.bindings)+len(state.ByNode))
	for alias, nodeID := range c.bindings {
		nodes[nodeID] = alias
	}
	for nodeID, payload := range state.ByNode {
		if nodeID != payload.NodeID || validateDirectoryPayload(payload, time.Unix(0, 0)) != nil {
			return fmt.Errorf("mesh directory: invalid cached node %s", nodeID)
		}
		if owner, exists := aliases[payload.Alias]; exists && owner != nodeID {
			return fmt.Errorf("mesh directory: conflicting cached alias %s", payload.Alias)
		}
		if knownAlias, exists := nodes[nodeID]; exists && knownAlias != payload.Alias {
			return fmt.Errorf("mesh directory: cached node %s has multiple aliases", nodeID)
		}
		aliases[payload.Alias] = nodeID
		nodes[nodeID] = payload.Alias
		if owner, exists := c.bindings[payload.Alias]; exists && owner != nodeID {
			return fmt.Errorf("mesh directory: alias binding changed for %s", payload.Alias)
		}
		if current, exists := c.byNode[nodeID]; exists && current.Alias != payload.Alias {
			return fmt.Errorf("mesh directory: cached alias changed for %s", nodeID)
		}
	}
	for alias, nodeID := range state.Bindings {
		if alias != normalizeDirectoryName(alias) || !validDirectoryName(alias) ||
			nodeID != strings.ToLower(nodeID) || !ValidNodeID(nodeID) {
			return fmt.Errorf("mesh directory: invalid cached binding %s", alias)
		}
		if owner, exists := aliases[alias]; exists && owner != nodeID {
			return fmt.Errorf("mesh directory: conflicting cached binding %s", alias)
		}
		if knownAlias, exists := nodes[nodeID]; exists && knownAlias != alias {
			return fmt.Errorf("mesh directory: cached node %s has multiple aliases", nodeID)
		}
		nodes[nodeID] = alias
		if owner, exists := c.bindings[alias]; exists && owner != nodeID {
			return fmt.Errorf("mesh directory: alias binding changed for %s", alias)
		}
	}
	for nodeID := range state.Highest {
		if nodeID != strings.ToLower(nodeID) || !ValidNodeID(nodeID) {
			return fmt.Errorf("mesh directory: invalid cached high-water node %s", nodeID)
		}
	}

	for alias, nodeID := range state.Bindings {
		c.bindings[alias] = nodeID
	}
	for nodeID, payload := range state.ByNode {
		if current, exists := c.byNode[nodeID]; !exists || payload.Sequence > current.Sequence {
			c.byNode[nodeID] = payload
			c.byAlias[payload.Alias] = payload
		}
		c.bindings[payload.Alias] = nodeID
		if c.highest[nodeID] < payload.Sequence {
			c.highest[nodeID] = payload.Sequence
		}
	}
	for nodeID, sequence := range state.Highest {
		if c.highest[nodeID] < sequence {
			c.highest[nodeID] = sequence
		}
	}
	return nil
}

func (c *DirectoryClient) ResolveNode(ctx context.Context, nodeID string) (DirectoryPayload, error) {
	nodeID = strings.ToLower(nodeID)
	c.mu.Lock()
	if payload, ok := c.byNode[nodeID]; ok && payload.ExpiresAt > time.Now().Unix() {
		c.mu.Unlock()
		return payload, nil
	}
	c.mu.Unlock()
	return c.fetch(ctx, "/v1/nodes/"+url.PathEscape(nodeID), nodeID, "")
}

func (c *DirectoryClient) ResolveAlias(ctx context.Context, alias string) (DirectoryPayload, error) {
	alias = normalizeDirectoryName(alias)
	c.mu.Lock()
	if payload, ok := c.byAlias[alias]; ok && payload.ExpiresAt > time.Now().Unix() {
		c.mu.Unlock()
		return payload, nil
	}
	c.mu.Unlock()
	return c.fetch(ctx, "/v1/aliases/"+url.PathEscape(alias), "", alias)
}

func (c *DirectoryClient) fetch(ctx context.Context, path, wantNode, wantAlias string) (DirectoryPayload, error) {
	// #nosec G704 -- baseURL is operator configuration validated as HTTPS (or
	// loopback HTTP), and path contains only fixed prefixes plus PathEscape.
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return DirectoryPayload{}, err
	}
	// #nosec G704 -- the default client rejects redirects, so the validated
	// operator-configured directory remains the only request destination.
	response, err := c.client.Do(request)
	if err != nil {
		return DirectoryPayload{}, fmt.Errorf("mesh directory: lookup: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return DirectoryPayload{}, &DirectoryHTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxDirectoryRecordBytes+1))
	if err != nil || len(body) > MaxDirectoryRecordBytes {
		return DirectoryPayload{}, errors.New("mesh directory: oversized response")
	}
	var record SignedDirectoryRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return DirectoryPayload{}, fmt.Errorf("mesh directory: decode response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DirectoryPayload{}, err
	}
	payload, err := VerifyDirectoryRecord(record, time.Now())
	if err != nil {
		return DirectoryPayload{}, err
	}
	if wantNode != "" && payload.NodeID != wantNode {
		return DirectoryPayload{}, errors.New("mesh directory: node lookup returned another identity")
	}
	if wantAlias != "" && payload.Alias != wantAlias {
		return DirectoryPayload{}, errors.New("mesh directory: alias lookup returned another name")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refreshPersistentStateLocked(payload.NodeID, payload.Alias); err != nil {
		return DirectoryPayload{}, err
	}
	if payload.Sequence < c.highest[payload.NodeID] {
		return DirectoryPayload{}, errors.New("mesh directory: sequence rollback")
	}
	if nodeID, ok := c.bindings[payload.Alias]; ok && nodeID != payload.NodeID {
		return DirectoryPayload{}, errors.New("mesh directory: alias binding changed")
	}
	if err := c.persistPayloadLocked(payload); err != nil {
		return DirectoryPayload{}, err
	}
	c.highest[payload.NodeID] = payload.Sequence
	c.bindings[payload.Alias] = payload.NodeID
	c.byNode[payload.NodeID] = payload
	c.byAlias[payload.Alias] = payload
	return payload, nil
}

// Publish signs and uploads the next monotonic record for id.
func (c *DirectoryClient) Publish(ctx context.Context, id *Identity, alias string, ttl time.Duration, lighthouses []LighthouseLocator) (DirectoryPayload, error) {
	if id == nil {
		return DirectoryPayload{}, errors.New("mesh directory: identity is required")
	}
	if ttl <= 0 {
		return DirectoryPayload{}, errors.New("mesh directory: record TTL must be positive")
	}
	alias = normalizeDirectoryName(alias)
	current, err := c.remotePayload(ctx, id.NodeID)
	if err != nil {
		var httpErr *DirectoryHTTPError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			current = DirectoryPayload{}
		} else {
			return DirectoryPayload{}, err
		}
	}
	c.mu.Lock()
	if err := c.refreshPersistentStateLocked(id.NodeID, alias); err != nil {
		c.mu.Unlock()
		return DirectoryPayload{}, err
	}
	if current.Sequence < c.highest[id.NodeID] {
		c.mu.Unlock()
		return DirectoryPayload{}, errors.New("mesh directory: sequence rollback")
	}
	if current.Sequence > 0 && current.Alias != alias {
		c.mu.Unlock()
		return DirectoryPayload{}, errors.New("mesh directory: alias is immutable")
	}
	if nodeID, ok := c.bindings[alias]; ok && nodeID != id.NodeID {
		c.mu.Unlock()
		return DirectoryPayload{}, errors.New("mesh directory: alias binding changed")
	}
	if current.Sequence == ^uint64(0) {
		c.mu.Unlock()
		return DirectoryPayload{}, errors.New("mesh directory: sequence exhausted")
	}
	c.mu.Unlock()
	payload := DirectoryPayload{
		Version: DirectoryRecordVersion, NodeID: id.NodeID, Alias: alias,
		Sequence: current.Sequence + 1, ExpiresAt: time.Now().Add(ttl).Unix(), Lighthouses: lighthouses,
	}
	record, err := SignDirectoryRecord(id, payload)
	if err != nil {
		return DirectoryPayload{}, err
	}
	body, err := json.Marshal(record)
	if err != nil {
		return DirectoryPayload{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/v1/records/"+url.PathEscape(id.NodeID), bytes.NewReader(body))
	if err != nil {
		return DirectoryPayload{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return DirectoryPayload{}, fmt.Errorf("mesh directory: publish: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return DirectoryPayload{}, &DirectoryHTTPError{StatusCode: response.StatusCode}
	}
	verified, err := VerifyDirectoryRecord(record, time.Now())
	if err != nil {
		return DirectoryPayload{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.refreshPersistentStateLocked(verified.NodeID, verified.Alias); err != nil {
		return DirectoryPayload{}, err
	}
	if err := c.persistPayloadLocked(verified); err != nil {
		return DirectoryPayload{}, err
	}
	c.bindings[verified.Alias] = verified.NodeID
	if c.highest[verified.NodeID] <= verified.Sequence {
		c.highest[verified.NodeID] = verified.Sequence
		c.byNode[verified.NodeID] = verified
		c.byAlias[verified.Alias] = verified
	}
	return verified, nil
}

func (c *DirectoryClient) remotePayload(ctx context.Context, nodeID string) (DirectoryPayload, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/v1/nodes/"+url.PathEscape(nodeID), nil)
	if err != nil {
		return DirectoryPayload{}, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return DirectoryPayload{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return DirectoryPayload{}, &DirectoryHTTPError{StatusCode: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxDirectoryRecordBytes+1))
	if err != nil || len(body) > MaxDirectoryRecordBytes {
		return DirectoryPayload{}, errors.New("mesh directory: oversized response")
	}
	var record SignedDirectoryRecord
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return DirectoryPayload{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return DirectoryPayload{}, err
	}
	payload, err := VerifyDirectoryRecord(record, time.Unix(0, 0))
	if err != nil {
		return DirectoryPayload{}, err
	}
	if payload.NodeID != nodeID {
		return DirectoryPayload{}, errors.New("mesh directory: node lookup returned another identity")
	}
	return payload, nil
}

func (c *DirectoryClient) persistPayloadLocked(payload DirectoryPayload) error {
	if c.db == nil {
		return nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("mesh directory: encode client state: %w", err)
	}
	result, err := c.db.Exec(
		`INSERT INTO directory_client_state(node_id, alias, payload, highest)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(node_id) DO UPDATE SET
			payload = excluded.payload,
			highest = excluded.highest
		 WHERE directory_client_state.alias = excluded.alias
		   AND excluded.highest >= directory_client_state.highest`,
		payload.NodeID, payload.Alias, raw, encodeDirectorySequence(payload.Sequence),
	)
	if err != nil {
		return fmt.Errorf("mesh directory: persist client state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mesh directory: persist client state: %w", err)
	}
	if affected != 1 {
		return errors.New("mesh directory: persistent sequence or alias conflict")
	}
	return nil
}

func (c *DirectoryClient) refreshPersistentStateLocked(nodeID, alias string) error {
	if c.db == nil {
		return nil
	}
	state, err := readDirectoryClientState(c.db, nodeID, alias)
	if err != nil {
		return err
	}
	return c.mergeStateLocked(state)
}
