package bridge

const (
	defaultMaxSearchResults  = 50
	hardMaxSearchResults     = 250
	defaultMaxFolderResults  = 200
	hardMaxFolderResults     = 1000
	defaultMaxBodyBytes      = 256 * 1024
	hardMaxBodyBytes         = 1024 * 1024
	defaultMaxThreadMessages = 20
	hardMaxThreadMessages    = 100
	defaultMaxThreadFetches  = 30
	hardMaxThreadFetches     = 100
	defaultMaxPreviewChars   = 200
	hardMaxPreviewChars      = 1000
	defaultMaxOutputBytes    = 100000
	hardMaxOutputBytes       = 400000
	maxMailboxNameBytes      = 512
	maxSearchTermBytes       = 1024
)

// Bounds contains the effective finite limits used by bridge operations.
type Bounds struct {
	MaxSearchResults   int
	MaxFolderResults   int
	MaxBodyBytes       int
	MaxHeaderBytes     int
	MaxMimeParts       int
	MaxAttachmentCount int
	MaxThreadMessages  int
	MaxThreadFetches   int
	MaxPreviewChars    int
	MaxOutputBytes     int
}

// BoundsPatch applies caller-selected values over the immutable defaults.
type BoundsPatch struct {
	MaxSearchResults   *int `json:"maxSearchResults,omitempty"`
	MaxFolderResults   *int `json:"maxFolderResults,omitempty"`
	MaxBodyBytes       *int `json:"maxBodyBytes,omitempty"`
	MaxHeaderBytes     *int `json:"maxHeaderBytes,omitempty"`
	MaxMimeParts       *int `json:"maxMimeParts,omitempty"`
	MaxAttachmentCount *int `json:"maxAttachmentCount,omitempty"`
	MaxThreadMessages  *int `json:"maxThreadMessages,omitempty"`
	MaxThreadFetches   *int `json:"maxThreadFetches,omitempty"`
	MaxPreviewChars    *int `json:"maxPreviewChars,omitempty"`
	MaxOutputBytes     *int `json:"maxOutputBytes,omitempty"`
}

// Int returns a pointer suitable for a BoundsPatch literal.
func Int(value int) *int {
	return &value
}

func mergeBounds(patch BoundsPatch) (Bounds, error) {
	if patch.MaxHeaderBytes != nil && *patch.MaxHeaderBytes == 0 {
		return Bounds{}, errorCode(CodeBoundsExceeded)
	}

	var bounds Bounds
	for _, field := range [...]struct {
		input             *int
		output            *int
		fallback, ceiling int
	}{
		{patch.MaxSearchResults, &bounds.MaxSearchResults, defaultMaxSearchResults, hardMaxSearchResults},
		{patch.MaxFolderResults, &bounds.MaxFolderResults, defaultMaxFolderResults, hardMaxFolderResults},
		{patch.MaxBodyBytes, &bounds.MaxBodyBytes, defaultMaxBodyBytes, hardMaxBodyBytes},
		{patch.MaxHeaderBytes, &bounds.MaxHeaderBytes, defaultMaxHeaderBytes, maximumMaxHeaderBytes},
		{patch.MaxMimeParts, &bounds.MaxMimeParts, defaultMaxMIMEParts, maximumMaxMIMEParts},
		{patch.MaxAttachmentCount, &bounds.MaxAttachmentCount, defaultMaxAttachmentCount, maximumMaxAttachmentCount},
		{patch.MaxThreadMessages, &bounds.MaxThreadMessages, defaultMaxThreadMessages, hardMaxThreadMessages},
		{patch.MaxThreadFetches, &bounds.MaxThreadFetches, defaultMaxThreadFetches, hardMaxThreadFetches},
		{patch.MaxPreviewChars, &bounds.MaxPreviewChars, defaultMaxPreviewChars, hardMaxPreviewChars},
		{patch.MaxOutputBytes, &bounds.MaxOutputBytes, defaultMaxOutputBytes, hardMaxOutputBytes},
	} {
		if field.input == nil {
			*field.output = field.fallback
			continue
		}
		if *field.input < 0 {
			return Bounds{}, errorCode(CodeBoundsExceeded)
		}

		*field.output = min(*field.input, field.ceiling)
	}

	return bounds, nil
}
