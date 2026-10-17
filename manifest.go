// Package cli runs the alguna CLI. The command manifest describes every API
// operation as data; this package turns it into cobra commands and sends the
// requests. The command, cmd/alguna, embeds manifest.json and passes it to Main.
package cli

// Manifest is the whole command surface, written from the API spec with
// Forge's command model. Commands and
// Methods follow Forge's Schema.command and CommandMeta.
type Manifest struct {
	BaseURL    string    `json:"baseUrl"`
	APIVersion string    `json:"apiVersion"`
	Commands   []Command `json:"commands"`
}

// Command groups the methods of one API resource, such as `alguna customers`.
type Command struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Methods     []Method `json:"methods"`
}

// Method is one API operation and its command, such as `alguna customers list`.
type Method struct {
	Name        string `json:"name"`
	OperationID string `json:"operationId"`
	Summary     string `json:"summary"`
	Description string `json:"description,omitempty"`
	// Epilogue is printed after the help, such as an example.
	Epilogue   string `json:"epilogue,omitempty"`
	HTTPMethod string `json:"httpMethod"`
	// APIPath is the spec path template, such as /customers/{id}.
	APIPath string `json:"apiPath"`
	// Arguments are the path parameters, in the order they appear in APIPath.
	Arguments      []Argument `json:"arguments,omitempty"`
	Options        []Option   `json:"options,omitempty"`
	HasRequestBody bool       `json:"hasRequestBody,omitempty"`
	// RequestBodyRequired names every required top-level body property, also
	// the objects and lists that have no option.
	RequestBodyRequired []string `json:"requestBodyRequired,omitempty"`
	// RequireConfirmation is the warning shown before the request is sent.
	RequireConfirmation string      `json:"requireConfirmation,omitempty"`
	Pagination          *Pagination `json:"pagination,omitempty"`
	// Binary methods return a file, such as a PDF, not JSON.
	Binary bool `json:"binary,omitempty"`
	// IdentifiedByBody marks an operation that the API identifies by its body,
	// such as events by their unique_id (x-alguna-identified-by-body in the spec).
	// It gets no automatic Idempotency-Key: a new key on each run would replace
	// that identity, so a run repeated after a failure would act twice.
	IdentifiedByBody bool `json:"identifiedByBody,omitempty"`
}

// Argument is a path parameter, given as a positional argument.
type Argument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Option is a flag: a query parameter, or a top-level scalar property of the JSON
// body. Every body property can also be set with --body.
type Option struct {
	// Name is the flag name; Field is the wire name.
	Name        string     `json:"name"`
	Field       string     `json:"field"`
	In          string     `json:"in"`
	Type        OptionType `json:"type"`
	Required    bool       `json:"required,omitempty"`
	Description string     `json:"description,omitempty"`
	Enum        []string   `json:"enum,omitempty"`
}

const (
	inQuery = "query"
	inBody  = "body"
)

type OptionType string

const (
	typeString  OptionType = "string"
	typeInteger OptionType = "integer"
	typeNumber  OptionType = "number"
	typeBoolean OptionType = "boolean"
	// typeStringList is sent as one comma-separated query value.
	typeStringList OptionType = "string-list"
)

// Pagination names the query parameters and the response field of an offset-paginated list.
type Pagination struct {
	Offset  string `json:"offset"`
	Limit   string `json:"limit"`
	Results string `json:"results"`
	// MaxLimit is the spec maximum of Limit, the page size of --all.
	MaxLimit int `json:"maxLimit"`
}
