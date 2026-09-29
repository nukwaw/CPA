package usagepersist

import (
	"database/sql"
	"net/http"
)

// Options selects usage storage, independently of CPA's configuration/auth store.
// PostgresDSN opens an owned pool; an explicitly supplied Database is borrowed.
// DataDir selects a local journal when neither database option is supplied.
type Options struct {
	PostgresDSN string
	Database    *sql.DB
	Schema      string
	DataDir     string
	HTTPClient  *http.Client
}
