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
			name: "role name needing quotes",
			item: `some-role=c/postgres`,
			want: `GRANT CONNECT ON DATABASE "core" TO "some-role"`,
			ok:   true,
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
