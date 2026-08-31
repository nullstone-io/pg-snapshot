package restore

import (
	"context"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The hook's whole contract: the staging URL under both common names, and the resolved owner as
// OWNER_ROLE -- a deployment may only set the RESTORE_OWNER_ROLE alias, so the hook cannot read
// it from the ambient environment.
func TestMigratorEnvContract(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("needs sh on PATH")
	}

	err := Migrator{
		Command:     `test "$OWNER_ROLE" = app_owner -a "$DATABASE_URL" = url -a "$POSTGRES_URL" = url`,
		DatabaseURL: "url",
		Owner:       "app_owner",
	}.Run(context.Background())
	assert.NoError(t, err)
}
