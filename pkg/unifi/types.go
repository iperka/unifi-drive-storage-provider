package unifi

import "encoding/json"

// This file models the real UniFi Drive local API, reverse-engineered against a
// UNAS Pro (UniFi Drive 4.2.6 / firmware 5.1.15) by capturing the web UI's
// traffic. Two API styles coexist:
//
//   - v2 read endpoints (e.g. GET /proxy/drive/api/v2/drives, /storage) return
//     bare JSON objects.
//   - v1 endpoints (e.g. /proxy/drive/api/v1/shared, /services/nfs/...) wrap the
//     payload in an envelope: {"err":..., "type":"single|collection", "data":...}.
//
// envelope captures the v1 wrapper. err is non-nil on failure.
type envelope struct {
	Err  *apiError       `json:"err"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// apiError is the v1 error object.
type apiError struct {
	Msg      string          `json:"msg"`
	Code     string          `json:"code"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

func (e *apiError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code != "" {
		return e.Msg + " (" + e.Code + ")"
	}
	return e.Msg
}

// --- storage pools (GET /proxy/drive/api/v2/storage) ---

type storageResponse struct {
	Pools []storagePool `json:"pools"`
}

type storagePool struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Status string `json:"status"`
}

// --- shared drives (GET/POST /proxy/drive/api/v1/shared) ---
//
// The list response is an envelope of type "collection" wrapping []apiDrive;
// create is an envelope of type "single" wrapping one apiDrive.
type apiDrive struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	StoragePoolID string `json:"storagePoolId"`
	// Quota is the size limit in GB; -1 means unlimited.
	Quota  int64  `json:"quota"`
	Usage  int64  `json:"usage"`
	Status string `json:"status"`
}

// createDriveRequest is the body of POST /proxy/drive/api/v1/shared. The empty
// members/groups slices and security:"none" mirror what the UI sends for a
// plain shared drive (no per-user ACLs — access is governed by the NFS export).
type createDriveRequest struct {
	Name          string   `json:"name"`
	StoragePoolID string   `json:"storagePoolId"`
	Quota         int64    `json:"quota"`
	Members       []string `json:"members"`
	Groups        []string `json:"groups"`
	Security      string   `json:"security"`
}

// --- NFS export config (GET /proxy/drive/api/v1/services/nfs/advanced-settings) ---
//
// NFS access is modelled globally as a list of connections, one per client IP,
// each listing the shared drives that client may access and at what permission.
// Enabling NFS for a drive on a set of node IPs is a read-modify-write of this
// structure.
type nfsAdvancedSettings struct {
	Connections []nfsConnection `json:"connections"`
}

type nfsConnection struct {
	Client       string        `json:"client"`
	Async        bool          `json:"async"`
	ErrorCodes   []string      `json:"errorCodes"`
	SharedDrives []nfsDriveAcl `json:"sharedDrives"`
}

type nfsDriveAcl struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	EncryptionStatus string `json:"encryptionStatus"`
	// Permission is "rw" or "ro".
	Permission string `json:"permission"`
}

// nfsSettings is GET /proxy/drive/api/v1/services/nfs/settings -> {"enable":bool}.
type nfsSettings struct {
	Enable bool `json:"enable"`
}

// batchOperationRequest is the body of
// POST /proxy/drive/api/v1/systems/storage/shared/batch-operation. Shared
// drives are deleted by NAME (not id), via action "delete" (the UI first
// "deactivate"s them). Response data is the string "OK".
type batchOperationRequest struct {
	Action string   `json:"action"`
	Names  []string `json:"names"`
}

// --- auth ---

type csrfResponse struct {
	CSRFToken string `json:"csrfToken"`
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}
