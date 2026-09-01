package restore

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGrantFromAclItem(t *testing.T) {
	tests := []struct {
		name string
		item string
		want string
		ok   bool
	}{
		{
			name: "all three database privileges",
			item: "app_user=CTc/postgres",
			want: `GRANT CREATE, TEMPORARY, CONNECT ON DATABASE "core" TO "app_user"`,
			ok:   true,
		},
		{
			name: "connect only",
			item: "reader=c/postgres",
			want: `GRANT CONNECT ON DATABASE "core" TO "reader"`,
			ok:   true,
		},
		{
			// An empty grantee is PUBLIC, which is not an identifier and must not be quoted
			name: "public grantee",
			item: "=Tc/postgres",
			want: `GRANT TEMPORARY, CONNECT ON DATABASE "core" TO PUBLIC`,
			ok:   true,
		},
		{
			name: "grant option",
			item: "app_user=c*/postgres",
			want: `GRANT CONNECT ON DATABASE "core" TO "app_user" WITH GRANT OPTION`,
			ok:   true,
		},
		{
			// aclitem output quotes a grantee that needs it; the quotes are not part of the name
			name: "role name needing quotes",
			item: `"patterniq-devops-previews-poc-4412885477"=Tc/postgres`,
			want: `GRANT TEMPORARY, CONNECT ON DATABASE "core" TO "patterniq-devops-previews-poc-4412885477"`,
			ok:   true,
		},
		{
			// Embedded quotes come doubled inside the quoted grantee
			name: "role name containing a quote",
			item: `"we""ird"=c/postgres`,
			want: `GRANT CONNECT ON DATABASE "core" TO "we""ird"`,
			ok:   true,
		},
		{
			// An "=" inside a quoted grantee must not be mistaken for the separator
			name: "role name containing equals",
			item: `"a=b"=c/postgres`,
			want: `GRANT CONNECT ON DATABASE "core" TO "a=b"`,
			ok:   true,
		},
		{
			name: "unterminated quoted grantee",
			item: `"broken=c/postgres`,
			ok:   false,
		},
		{
			// Privileges of other object types carry letters that mean nothing on a database
			name: "no database privileges",
			item: "app_user=arwd/postgres",
			ok:   false,
		},
		{
			name: "malformed",
			item: "nonsense",
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := grantFromAclItem(tt.item, "core")
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}
