// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drivecli

import "encoding/json"

// RootSection is one virtual root emitted by `filesystem list / --json`.
type RootSection struct {
	Path string `json:"path"`
}

// Device is the frozen `/devices` listing object.
type Device struct {
	UID           string     `json:"uid"`
	Type          string     `json:"type"`
	Name          NameResult `json:"name"`
	RootFolderUID string     `json:"rootFolderUid"`
	CreationTime  string     `json:"creationTime"`
	LastSyncTime  string     `json:"lastSyncTime,omitempty"`
	ShareID       string     `json:"shareId"`
}

// NameResult is Result<string, Error|InvalidNameError>.
type NameResult struct {
	OK    bool             `json:"ok"`
	Value string           `json:"value,omitempty"`
	Error InvalidNameError `json:"error,omitempty"`
}

// InvalidNameError is the plain-object name failure shape.
type InvalidNameError struct {
	Name  string `json:"name,omitempty"`
	Error string `json:"error,omitempty"`
}

// AuthorResult is Result<string|null, UnverifiedAuthorError>.
type AuthorResult struct {
	OK    bool                  `json:"ok"`
	Value *string               `json:"value"`
	Error UnverifiedAuthorError `json:"error,omitempty"`
}

// UnverifiedAuthorError is the plain-object author failure shape.
type UnverifiedAuthorError struct {
	ClaimedAuthor *string `json:"claimedAuthor,omitempty"`
	Error         string  `json:"error,omitempty"`
}

// OwnedBy is the node owner object. Emails stay in decoded payloads only.
type OwnedBy struct {
	Email        string `json:"email,omitempty"`
	Organization string `json:"organization,omitempty"`
}

// Membership is node-local sharing membership.
type Membership struct {
	Role       string       `json:"role"`
	InviteTime string       `json:"inviteTime"`
	SharedBy   AuthorResult `json:"sharedBy"`
}

// ClaimedDigests is the optional revision digest object.
type ClaimedDigests struct {
	SHA1         string `json:"sha1,omitempty"`
	SHA1Verified bool   `json:"sha1Verified"`
}

// Revision is NodeEntity.activeRevision.
type Revision struct {
	UID                       string          `json:"uid"`
	State                     string          `json:"state"`
	CreationTime              string          `json:"creationTime"`
	ContentAuthor             AuthorResult    `json:"contentAuthor"`
	StorageSize               int64           `json:"storageSize"`
	IsImported                bool            `json:"isImported"`
	ClaimedSize               *int64          `json:"claimedSize,omitempty"`
	ClaimedModificationTime   string          `json:"claimedModificationTime,omitempty"`
	ClaimedDigests            *ClaimedDigests `json:"claimedDigests,omitempty"`
	ClaimedAdditionalMetadata json.RawMessage `json:"claimedAdditionalMetadata,omitempty"`
}

// FolderInfo is NodeEntity.folder.
type FolderInfo struct {
	ClaimedModificationTime string `json:"claimedModificationTime,omitempty"`
	IsImported              bool   `json:"isImported"`
}

// NodeEntity is the frozen filesystem info/list node object.
type NodeEntity struct {
	UID               string          `json:"uid"`
	ParentUID         string          `json:"parentUid,omitempty"`
	Name              NameResult      `json:"name"`
	KeyAuthor         AuthorResult    `json:"keyAuthor"`
	NameAuthor        AuthorResult    `json:"nameAuthor"`
	DirectRole        string          `json:"directRole"`
	Membership        *Membership     `json:"membership,omitempty"`
	OwnedBy           OwnedBy         `json:"ownedBy"`
	Type              string          `json:"type"`
	MediaType         string          `json:"mediaType,omitempty"`
	IsShared          bool            `json:"isShared"`
	IsSharedByURL     bool            `json:"isSharedByUrl"`
	DeprecatedShareID string          `json:"deprecatedShareId,omitempty"`
	CreationTime      string          `json:"creationTime"`
	ModificationTime  string          `json:"modificationTime"`
	TrashTime         string          `json:"trashTime,omitempty"`
	TotalStorageSize  *int64          `json:"totalStorageSize,omitempty"`
	ActiveRevision    *Revision       `json:"activeRevision,omitempty"`
	Folder            *FolderInfo     `json:"folder,omitempty"`
	TreeEventScopeID  string          `json:"treeEventScopeId"`
	Errors            json.RawMessage `json:"errors,omitempty"`
}

// ListResult holds exactly one frozen list shape.
type ListResult struct {
	Sections []RootSection
	Devices  []Device
	Nodes    []NodeEntity
}

// Member is one ShareResult invitation or member.
type Member struct {
	UID            string       `json:"uid"`
	InvitationTime string       `json:"invitationTime"`
	AddedByEmail   AuthorResult `json:"addedByEmail"`
	InviteeEmail   string       `json:"inviteeEmail"`
	Role           string       `json:"role"`
	State          string       `json:"state,omitempty"`
}

// URLAccess is the optional public-link object.
type URLAccess struct {
	UID                          string `json:"uid"`
	CreationTime                 string `json:"creationTime"`
	Role                         string `json:"role"`
	URL                          string `json:"url"`
	CustomPassword               string `json:"customPassword,omitempty"`
	ExpirationTime               string `json:"expirationTime,omitempty"`
	NumberOfInitializedDownloads int64  `json:"numberOfInitializedDownloads"`
}

// ShareResult is the frozen sharing-status object.
type ShareResult struct {
	ProtonInvitations    []Member   `json:"protonInvitations"`
	NonProtonInvitations []Member   `json:"nonProtonInvitations"`
	Members              []Member   `json:"members"`
	URLAccess            *URLAccess `json:"urlAccess,omitempty"`
	EditorsCanShare      bool       `json:"editorsCanShare"`
}

// SharingStatus is either a ShareResult or the CLI's literal undefined.
type SharingStatus struct {
	Shared bool
	Info   *ShareResult
}

// DownloadFailure is one download summary failure object.
type DownloadFailure struct {
	Name    string `json:"name"`
	NodeUID string `json:"nodeUid,omitempty"`
	Error   string `json:"error"`
}

// DownloadSummary is the frozen download --json object.
type DownloadSummary struct {
	TransferredItems int64             `json:"transferredItems"`
	TransferredBytes int64             `json:"transferredBytes"`
	SkippedItems     int64             `json:"skippedItems"`
	FailedItems      int64             `json:"failedItems"`
	Failures         []DownloadFailure `json:"failures"`
}
