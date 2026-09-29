package usagepersist

import (
	"database/sql"
	"net/http"
)

// Options selects the existing application's storage resource. Database is
// borrowed from CPA's PostgreSQL store and is never closed by this component.
// DataDir is an explicit, resolved writable location for a local journal.
// These are construction parameters, not new environment/configuration flags.
type Options struct {
	Database   *sql.DB
	Schema     string
	DataDir    string
	HTTPClient *http.Client
}
