package main

import (
	"context"
	"errors"
	"testing"
)

func TestNormalizeRole(t *testing.T) {
	cases := []struct {
		raw     string
		want    Role
		wantErr bool
	}{
		{raw: "admin", want: RoleAdmin},
		{raw: "  User  ", want: RoleUser},
		{raw: "READONLY", want: RoleReadOnly},
		{raw: "", wantErr: true},
		{raw: "superadmin", wantErr: true},
	}
	for _, tc := range cases {
		got, err := normalizeRole(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normalizeRole(%q): expected an error, got %q", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeRole(%q): unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeRole(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCreateUserRejectsCaseInsensitiveDuplicate(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if err := app.createUser(ctx, "Alice", "hash1", RoleUser); err != nil {
		t.Fatalf("create first user: %v", err)
	}
	if err := app.createUser(ctx, "alice", "hash2", RoleAdmin); !errors.Is(err, ErrUserExists) {
		t.Fatalf("expected ErrUserExists for a case-insensitive duplicate, got %v", err)
	}

	users, err := app.listUsers(ctx)
	if err != nil {
		t.Fatalf("listUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("expected exactly 1 user after a rejected duplicate, got %d", len(users))
	}
}

func TestMigrateLegacyAdminPassword(t *testing.T) {
	t.Run("fresh install is a no-op", func(t *testing.T) {
		app := newTestApp(t)
		if err := app.migrateLegacyAdminPassword(context.Background()); err != nil {
			t.Fatalf("migrate on fresh install: %v", err)
		}
		configured, err := app.usersConfigured(context.Background())
		if err != nil {
			t.Fatalf("usersConfigured: %v", err)
		}
		if configured {
			t.Fatal("expected no users to be created on a fresh install")
		}
	})

	t.Run("upgrade creates an admin user from the legacy hash", func(t *testing.T) {
		app := newTestApp(t)
		settings := app.currentRuntimeSettings()
		settings.AdminPasswordHash = "legacy-hash-value"
		if err := app.applyRuntimeSettings(settings); err != nil {
			t.Fatalf("applyRuntimeSettings: %v", err)
		}
		if err := app.migrateLegacyAdminPassword(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		user, found, err := app.getUserByUsername(context.Background(), "admin")
		if err != nil {
			t.Fatalf("getUserByUsername: %v", err)
		}
		if !found {
			t.Fatal("expected an admin user to be created from the legacy password hash")
		}
		if user.Role != RoleAdmin {
			t.Errorf("migrated user role = %q, want %q", user.Role, RoleAdmin)
		}
		if user.PasswordHash != "legacy-hash-value" {
			t.Error("migrated user should reuse the legacy hash verbatim, not re-hash it")
		}
	})

	t.Run("no-op once a user already exists", func(t *testing.T) {
		app := newTestApp(t)
		if err := app.createUser(context.Background(), "someone", "h", RoleUser); err != nil {
			t.Fatalf("create user: %v", err)
		}
		settings := app.currentRuntimeSettings()
		settings.AdminPasswordHash = "legacy-hash-value"
		if err := app.applyRuntimeSettings(settings); err != nil {
			t.Fatalf("applyRuntimeSettings: %v", err)
		}
		if err := app.migrateLegacyAdminPassword(context.Background()); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		users, err := app.listUsers(context.Background())
		if err != nil {
			t.Fatalf("listUsers: %v", err)
		}
		if len(users) != 1 {
			t.Fatalf("expected the migration to stay a no-op once a user exists, got %d users", len(users))
		}
	})
}

func TestAssertNotLastEnabledAdmin(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	if err := app.createUser(ctx, "admin1", "h", RoleAdmin); err != nil {
		t.Fatalf("create admin1: %v", err)
	}
	admin1, _, err := app.getUserByUsername(ctx, "admin1")
	if err != nil {
		t.Fatalf("getUserByUsername: %v", err)
	}

	if err := app.assertNotLastEnabledAdmin(ctx, admin1); err == nil {
		t.Error("expected an error: admin1 is the only enabled admin")
	}

	if err := app.createUser(ctx, "admin2", "h", RoleAdmin); err != nil {
		t.Fatalf("create admin2: %v", err)
	}
	if err := app.assertNotLastEnabledAdmin(ctx, admin1); err != nil {
		t.Errorf("with a second enabled admin present, expected no error, got %v", err)
	}

	// A non-admin or already-disabled target is never "the last admin"
	// being removed, so the guard is a no-op for it regardless of count.
	if err := app.createUser(ctx, "regular", "h", RoleUser); err != nil {
		t.Fatalf("create regular user: %v", err)
	}
	regular, _, err := app.getUserByUsername(ctx, "regular")
	if err != nil {
		t.Fatalf("getUserByUsername: %v", err)
	}
	if err := app.assertNotLastEnabledAdmin(ctx, regular); err != nil {
		t.Errorf("non-admin target should never trip the guard, got %v", err)
	}
}
