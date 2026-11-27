package vault

import "ytcloud/internal/crypto"

// VaultHeader is stored unencrypted (only salt + wrapped DEK + vault metadata) at the top of vault.ytdb.
type VaultHeader struct {
	Magic     string            `json:"magic"` // "YTCLOUD_VAULT_V1"
	VaultID   string            `json:"vault_id"`
	VaultName string            `json:"vault_name"`
	KeyBundle *crypto.KeyBundle `json:"key_bundle"`
}

// VaultData is the full catalog state, compressed (zstd) and encrypted with AES-256-GCM using DEK.
type VaultData struct {
	Version          int                         `json:"version"`
	SettingsVersion  int                         `json:"settings_version,omitempty"`
	VaultID          string                      `json:"vault_id"`
	VaultName        string                      `json:"vault_name"`
	CatalogSeq       int                         `json:"catalog_seq"`
	DataRevision     int                         `json:"data_revision"`
	SyncedRevision   int                         `json:"synced_revision"`
	LastCatalogVID   string                      `json:"last_catalog_vid,omitempty"`
	PrevCatalogVID   string                      `json:"prev_catalog_vid,omitempty"`
	Settings         VaultSettings               `json:"settings"`
	Folders          map[string]*Folder          `json:"folders"`
	Collections      map[string]*Collection      `json:"collections"`
	Files            map[string]*FileEntry       `json:"files"`
	Containers       map[string]*VideoContainer  `json:"containers"`
	SyncGroups       map[string]*SyncGroup       `json:"sync_groups,omitempty"`
	PendingDownloads map[string]*PendingDownload `json:"pending_downloads,omitempty"`
	YouTubeAuth      *YouTubeOAuthState          `json:"youtube_auth,omitempty"`
}

// PendingDownload persists an active/interrupted download so yt-dlp (--continue) can resume on restart!
type PendingDownload struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Kind               string   `json:"kind"` // "container" or "peek"
	SourceURL          string   `json:"source_url,omitempty"`
	ContainerID        string   `json:"container_id,omitempty"`
	FileIDs            []string `json:"file_ids,omitempty"`
	SelectedRelPaths   []string `json:"selected_rel_paths,omitempty"`
	DestDir            string   `json:"dest_dir"`
	ApplyEditedMeta    bool     `json:"apply_edited_meta"`
	SkipUnchangedLocal bool     `json:"skip_unchanged_local"`
	DownloadDir        string   `json:"download_dir"`
	LastProgress       int      `json:"last_progress"`
	LastDetail         string   `json:"last_detail"`
	CreatedAt          string   `json:"created_at"`
}

// SyncGroup tracks an incremental backup chain for a local folder mapped to a virtual folder.
type SyncGroup struct {
	ID              string `json:"id"` // e.g. "SYNC-a4f8c219"
	Name            string `json:"name"`
	LocalPath       string `json:"local_path"`
	FolderID        string `json:"folder_id"`
	BaseContainerID string `json:"base_container_id"`
	LastContainerID string `json:"last_container_id"`
	DeltaCount      int    `json:"delta_count"` // 0 = Base only, 1..MaxDeltas = incremental update videos
	MaxDeltas       int    `json:"max_deltas"`  // default 10
	LastSyncedAt    string `json:"last_synced_at"`
}

type VaultSettings struct {
	DefaultResolution    string `json:"default_resolution"`     // "2k", "4k", "1080p", "720p"
	DefaultProfile       string `json:"default_profile"`        // "y8-gray", "y6-gray", "y4-gray", "art-stego", etc.
	DefaultVideoCodec    string `json:"default_video_codec,omitempty"` // "auto" (AV1 p/ dados, H.264 p/ arte), "av1", "h264"
	DefaultFPS           int    `json:"default_fps"`            // 15
	MaxRAMMB             int    `json:"max_ram_mb"`             // Go memory limit in MB (default 512)
	CPUThreads           int    `json:"cpu_threads"`            // FFmpeg/FEC CPU thread cap (default 2)
	MaxCPUPercent        int    `json:"max_cpu_percent,omitempty"` // 10..100% of system logical CPUs
	MaxRAMPercent        int    `json:"max_ram_percent,omitempty"` // 10..85% of system physical RAM
	PerfPreset           string `json:"perf_preset,omitempty"`     // "max", "balanced", "min", "custom"
	BackgroundMode       bool   `json:"background_mode"`        // run at BELOW_NORMAL Windows priority (default true)
	CleanupPolicy        string `json:"cleanup_policy"`         // "safe_after_verify", "immediate_after_upload", "keep"
	CatalogKeepPrevious  bool   `json:"catalog_keep_previous"`  // keep N-1 rollback Video-Index or delete immediately
	CatalogIncludeThumbs bool   `json:"catalog_include_thumbs"` // include previews in Video-Index backup
	OptimizePNGLossless  bool   `json:"optimize_png_lossless"`  // pixel-exact WebP Lossless re-encoding for PNGs (-38% to -97%)
	MaxDeltaVideos       *int   `json:"max_delta_videos,omitempty"` // 0 = full videos only, 1..99 = max delta updates (nil -> default 10)
	DefaultPrivacy       string `json:"default_privacy"`        // "public", "unlisted" or "private"
	Theme                string `json:"theme,omitempty"`        // "paper" (default), "night", "desert", "light", "rainbow", "twilight"
	Language             string `json:"language,omitempty"`     // "pt", "en", "es", "ru", "fr", "it", "ja", "ar", "zh"
	YtDlpCookiesBrowser  string `json:"ytdlp_cookies_browser,omitempty"`
	YtDlpCookiesFile     string `json:"ytdlp_cookies_file,omitempty"`
	YouTubeClientID      string `json:"youtube_client_id,omitempty"`
	YouTubeClientSecret  string `json:"youtube_client_secret,omitempty"`
	StagingDir           string `json:"staging_dir,omitempty"`
}

// EffectiveMaxDeltas returns the configured delta limit (0..99), defaulting to 10 for legacy vaults.
func (s VaultSettings) EffectiveMaxDeltas() int {
	if s.MaxDeltaVideos == nil {
		return 10
	}
	v := *s.MaxDeltaVideos
	if v < 0 {
		return 0
	}
	if v > 99 {
		return 99
	}
	return v
}

type YouTubeOAuthState struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiryUnix   int64  `json:"expiry_unix,omitempty"`
	ChannelTitle string `json:"channel_title,omitempty"`
}

type Folder struct {
	ID           string `json:"id"`
	ParentID     string `json:"parent_id"` // "" for root
	Name         string `json:"name"`
	SortOrder    int    `json:"sort_order,omitempty"`     // sibling ordering within the same ParentID
	FolderKeyHex string `json:"folder_key_hex,omitempty"` // 32-byte random per-folder encryption key (stored encrypted inside vault)
	CreatedAt    string `json:"created_at"`
}

type Collection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Color       string `json:"color"`
	CreatedAt   string `json:"created_at"`
}

type FileEntry struct {
	ID              string            `json:"id"`
	FolderID        string            `json:"folder_id"`
	ContainerID     string            `json:"container_id"`
	RelPathInVideo  string            `json:"rel_path_in_video"`
	OriginalName    string            `json:"original_name"`
	DisplayName     string            `json:"display_name"`
	MimeType        string            `json:"mime_type"`
	SizeBytes       int64             `json:"size_bytes"`
	SHA256          string            `json:"sha256"`
	OriginalDate    string            `json:"original_date"`
	EffectiveDate   string            `json:"effective_date"` // editable! e.g. "1998-07-01"
	DatePrecision   string            `json:"date_precision"` // "exact", "month", "year", "decade", "unknown"
	LocationLabel   string            `json:"location_label"` // e.g. "Praia Grande, SP"
	Notes           string            `json:"notes"`
	CollectionIDs   []string          `json:"collection_ids"`
	Tags            []string          `json:"tags"`
	PreviewKind     string            `json:"preview_kind"` // "image", "video", "text", "none"
	PreviewBase64   string            `json:"preview_base64,omitempty"`
	LocalSourcePath   string            `json:"local_source_path,omitempty"` // enables explicit "Liberar espaço local"
	LastExtractedPath string            `json:"last_extracted_path,omitempty"`
	LocalFreed        bool              `json:"local_freed"`
	ExtraMetadata   map[string]string `json:"extra_metadata,omitempty"`
	AddedAt         string            `json:"added_at"`
}

type VideoContainer struct {
	ID             string `json:"id"`
	ShortCode      string `json:"short_code"`       // e.g. "YTC-8f91a2c4" (opaque YouTube title)
	YouTubeVideoID string `json:"youtube_video_id"` // e.g. "dQw4w9WgXcQ"
	YouTubeURL     string `json:"youtube_url"`
	LocalVideoPath string `json:"local_video_path,omitempty"`
	LocalDeleted   bool   `json:"local_deleted"`
	Resolution     string `json:"resolution"`
	ProfileCode    string `json:"profile_code"`
	FPS            int    `json:"fps"`
	TotalFrames    int    `json:"total_frames"`
	ManifestFrames int    `json:"manifest_frames"`
	VideoSizeBytes int64  `json:"video_size_bytes"`
	DataSizeBytes  int64  `json:"data_size_bytes"`
	FileCount      int    `json:"file_count"`
	Status          string   `json:"status"` // "staged", "uploaded", "verified", "error"
	Privacy         string   `json:"privacy"`
	ContainerSalt   []byte   `json:"container_salt"`            // 16-byte HKDF salt for per-video encryption key
	KeyScope        string   `json:"key_scope,omitempty"`       // "" = legacy vault master DEK, "folder" = per-folder key
	FolderID        string   `json:"folder_id,omitempty"`       // virtual folder that owns this container's key
	FolderKeyHex    string   `json:"folder_key_hex,omitempty"`  // immutable copy of the 32-byte folder key used to encrypt this container
	SyncID          string   `json:"sync_id,omitempty"`
	DeltaIndex      int      `json:"delta_index,omitempty"` // 0 = base, 1..10 = update video
	ParentVideoID   string   `json:"parent_video_id,omitempty"`
	DeletedRelPaths []string `json:"deleted_rel_paths,omitempty"`
	CreatedAt       string   `json:"created_at"`
	VerifiedAt      string   `json:"verified_at,omitempty"`
}

type SearchQuery struct {
	Query        string `json:"query"`
	FolderID     string `json:"folder_id"`
	Recursive    bool   `json:"recursive"`
	CollectionID string `json:"collection_id"`
	Tag          string `json:"tag"`
	DateFrom     string `json:"date_from"`
	DateTo       string `json:"date_to"`
	OnlyNoDate   bool   `json:"only_no_date"`
	MediaType    string `json:"media_type"` // "", "image", "video", "document", "audio"
	ContainerID  string `json:"container_id"`
	SortBy       string `json:"sort_by"` // "date_desc", "date_asc", "name_asc", "size_desc", "added_desc"
}

type BatchMetadataUpdate struct {
	FileIDs        []string `json:"file_ids"`
	EffectiveDate  *string  `json:"effective_date,omitempty"`
	DatePrecision  *string  `json:"date_precision,omitempty"`
	ShiftDays      int      `json:"shift_days,omitempty"`
	LocationLabel  *string  `json:"location_label,omitempty"`
	AddTags        []string `json:"add_tags,omitempty"`
	AddCollection  string   `json:"add_collection,omitempty"`
	MoveToFolderID *string  `json:"move_to_folder_id,omitempty"`
}
