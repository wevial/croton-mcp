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

func defaultBounds() Bounds {
	return Bounds{
		MaxSearchResults:   defaultMaxSearchResults,
		MaxFolderResults:   defaultMaxFolderResults,
		MaxBodyBytes:       defaultMaxBodyBytes,
		MaxHeaderBytes:     defaultMaxHeaderBytes,
		MaxMimeParts:       defaultMaxMIMEParts,
		MaxAttachmentCount: defaultMaxAttachmentCount,
		MaxThreadMessages:  defaultMaxThreadMessages,
		MaxThreadFetches:   defaultMaxThreadFetches,
		MaxPreviewChars:    defaultMaxPreviewChars,
		MaxOutputBytes:     defaultMaxOutputBytes,
	}
}

func mergeBounds(patch BoundsPatch) (Bounds, error) {
	bounds := defaultBounds()

	var err error
	if bounds.MaxSearchResults, err = clampBound(patch.MaxSearchResults, bounds.MaxSearchResults, hardMaxSearchResults); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxFolderResults, err = clampBound(patch.MaxFolderResults, bounds.MaxFolderResults, hardMaxFolderResults); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxBodyBytes, err = clampBound(patch.MaxBodyBytes, bounds.MaxBodyBytes, hardMaxBodyBytes); err != nil {
		return Bounds{}, err
	}
	if patch.MaxHeaderBytes != nil && *patch.MaxHeaderBytes == 0 {
		return Bounds{}, errorCode(CodeBoundsExceeded)
	}
	if bounds.MaxHeaderBytes, err = clampBound(patch.MaxHeaderBytes, bounds.MaxHeaderBytes, maximumMaxHeaderBytes); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxMimeParts, err = clampBound(patch.MaxMimeParts, bounds.MaxMimeParts, maximumMaxMIMEParts); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxAttachmentCount, err = clampBound(patch.MaxAttachmentCount, bounds.MaxAttachmentCount, maximumMaxAttachmentCount); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxThreadMessages, err = clampBound(patch.MaxThreadMessages, bounds.MaxThreadMessages, hardMaxThreadMessages); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxThreadFetches, err = clampBound(patch.MaxThreadFetches, bounds.MaxThreadFetches, hardMaxThreadFetches); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxPreviewChars, err = clampBound(patch.MaxPreviewChars, bounds.MaxPreviewChars, hardMaxPreviewChars); err != nil {
		return Bounds{}, err
	}
	if bounds.MaxOutputBytes, err = clampBound(patch.MaxOutputBytes, bounds.MaxOutputBytes, hardMaxOutputBytes); err != nil {
		return Bounds{}, err
	}

	return bounds, nil
}

func clampBound(value *int, fallback, ceiling int) (int, error) {
	if value == nil {
		return fallback, nil
	}
	if *value < 0 {
		return 0, errorCode(CodeBoundsExceeded)
	}
	if *value > ceiling {
		return ceiling, nil
	}
	return *value, nil
}
