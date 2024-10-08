package migration

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"git.handmade.network/hmn/hmn/src/config"
	"git.handmade.network/hmn/hmn/src/db"
	"git.handmade.network/hmn/hmn/src/migration/migrations"
	"git.handmade.network/hmn/hmn/src/migration/types"
	"git.handmade.network/hmn/hmn/src/website"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/tracelog"
	"github.com/spf13/cobra"
)

var listMigrations bool

func init() {
	dbCommand := &cobra.Command{
		Use:   "db",
		Short: "Database-related commands",
	}

	migrateCommand := &cobra.Command{
		Use:   "migrate [target migration id]",
		Short: "Run database migrations",
		Run: func(cmd *cobra.Command, args []string) {
			if listMigrations {
				ListMigrations()
				return
			}

			targetVersion := time.Time{}
			if len(args) > 0 {
				var err error
				targetVersion, err = time.Parse(time.RFC3339, args[0])
				if err != nil {
					fmt.Printf("ERROR: bad version string: %v", err)
					os.Exit(1)
				}
			}
			Migrate(types.MigrationVersion(targetVersion))
		},
	}
	migrateCommand.Flags().BoolVar(&listMigrations, "list", false, "List available migrations")

	rollbackCommand := &cobra.Command{
		Use:   "rollback",
		Short: "Roll back the most recent completed migration",
		Run: func(cmd *cobra.Command, args []string) {
			Rollback()
		},
	}

	makeMigrationCommand := &cobra.Command{
		Use:   "makemigration <name> <description>...",
		Short: "Create a new database migration file",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) < 2 {
				fmt.Printf("You must provide a name and a description.\n\n")
				cmd.Usage()
				os.Exit(1)
			}

			name := args[0]
			description := strings.Join(args[1:], " ")

			MakeMigration(name, description)
		},
	}

	seedCommand := &cobra.Command{
		Use:   "seed",
		Short: "Resets the db and populates it with sample data.",
		Run: func(cmd *cobra.Command, args []string) {
			ResetDB()
			SampleSeed()
		},
	}

	seedFromFileCommand := &cobra.Command{
		Use:   "seedfile <filename>",
		Short: "Resets the db and runs the seed file.",
		Run: func(cmd *cobra.Command, args []string) {
			if len(args) < 1 {
				fmt.Printf("You must provide a seed file.\n\n")
				cmd.Usage()
				os.Exit(1)
			}

			ResetDB()
			SeedFromFile(args[0])
		},
	}

	website.WebsiteCommand.AddCommand(dbCommand)
	dbCommand.AddCommand(migrateCommand)
	dbCommand.AddCommand(rollbackCommand)
	dbCommand.AddCommand(makeMigrationCommand)
	dbCommand.AddCommand(seedCommand)
	dbCommand.AddCommand(seedFromFileCommand)
}

func getSortedMigrationVersions() []types.MigrationVersion {
	var allVersions []types.MigrationVersion
	for migrationTime, _ := range migrations.All {
		allVersions = append(allVersions, migrationTime)
	}
	sort.Slice(allVersions, func(i, j int) bool {
		return allVersions[i].Before(allVersions[j])
	})

	return allVersions
}

func getCurrentVersion(ctx context.Context, conn *pgx.Conn) (types.MigrationVersion, error) {
	var currentVersion time.Time
	row := conn.QueryRow(ctx, "SELECT version FROM hmn_migration")
	err := row.Scan(&currentVersion)
	if err != nil {
		return types.MigrationVersion{}, err
	}
	currentVersion = currentVersion.UTC()

	return types.MigrationVersion(currentVersion), nil
}

func tryGetCurrentVersion(ctx context.Context) types.MigrationVersion {
	defer func() {
		recover() // NOTE(ben): wat
	}()

	conn := db.NewConn()
	defer conn.Close(ctx)

	currentVersion, _ := getCurrentVersion(ctx, conn)

	return currentVersion
}

func ListMigrations() {
	ctx := context.Background()

	currentVersion := tryGetCurrentVersion(ctx)
	for _, version := range getSortedMigrationVersions() {
		migration := migrations.All[version]
		indicator := "  "
		if version.Equal(currentVersion) {
			indicator = "✔ "
		}
		fmt.Printf("%s%v (%s: %s)\n", indicator, version, migration.Name(), migration.Description())
	}
}

func LatestVersion() types.MigrationVersion {
	allVersions := getSortedMigrationVersions()
	return allVersions[len(allVersions)-1]
}

// Migrates either forward or backward to the selected migration version. You probably want to
// use LatestVersion to get the most recent migration.
func Migrate(targetVersion types.MigrationVersion) {
	ctx := context.Background() // In the future, this could actually do something cool.

	conn := db.NewConnWithConfig(config.PostgresConfig{
		LogLevel: tracelog.LogLevelWarn,
	})
	defer conn.Close(ctx)

	// create migration table
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS hmn_migration (
			version		TIMESTAMP WITH TIME ZONE
		)
	`)
	if err != nil {
		panic(fmt.Errorf("failed to create migration table: %w", err))
	}

	// ensure there is a row
	row := conn.QueryRow(ctx, "SELECT COUNT(*) FROM hmn_migration")
	var numRows int
	err = row.Scan(&numRows)
	if err != nil {
		panic(err)
	}
	if numRows < 1 {
		_, err := conn.Exec(ctx, "INSERT INTO hmn_migration (version) VALUES ($1)", time.Time{})
		if err != nil {
			panic(fmt.Errorf("failed to insert initial migration row: %w", err))
		}
	}

	// run migrations
	currentVersion, err := getCurrentVersion(ctx, conn)
	if err != nil {
		panic(fmt.Errorf("failed to get current version: %w", err))
	}
	if currentVersion.IsZero() {
		fmt.Println("This is the first time you have run database migrations.")
	} else {
		fmt.Printf("Current version: %s\n", currentVersion.String())
	}

	allVersions := getSortedMigrationVersions()
	if targetVersion.IsZero() {
		targetVersion = LatestVersion()
	}

	currentIndex := -1
	targetIndex := -1
	for i, version := range allVersions {
		if currentVersion.Equal(version) {
			currentIndex = i
		}
		if targetVersion.Equal(version) {
			targetIndex = i
		}
	}

	if targetIndex < 0 {
		fmt.Printf("ERROR: Could not find migration with version %v\n", targetVersion)
		return
	}

	if currentIndex < targetIndex {
		// roll forward
		for i := currentIndex + 1; i <= targetIndex; i++ {
			version := allVersions[i]
			migration := migrations.All[version]
			fmt.Printf("Applying migration %v (%v)\n", version, migration.Name())

			tx, err := conn.Begin(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to start transaction: %w", err))
			}
			defer tx.Rollback(ctx)

			err = migration.Up(ctx, tx)
			if err != nil {
				fmt.Printf("MIGRATION FAILED for migration %v.\n", version)
				fmt.Printf("Error: %v\n", err)
				return
			}

			_, err = tx.Exec(ctx, "UPDATE hmn_migration SET version = $1", version)
			if err != nil {
				panic(fmt.Errorf("failed to update version in migrations table: %w", err))
			}

			err = tx.Commit(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to commit transaction: %w", err))
			}
		}
	} else if currentIndex > targetIndex {
		// roll back
		for i := currentIndex; i > targetIndex; i-- {
			version := allVersions[i]
			previousVersion := types.MigrationVersion{}
			if i > 0 {
				previousVersion = allVersions[i-1]
			}

			tx, err := conn.Begin(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to start transaction: %w", err))
			}
			defer tx.Rollback(ctx)

			migration := migrations.All[version]
			fmt.Printf("Rolling back migration %v (%s)\n", migration.Version(), migration.Name())
			err = migration.Down(ctx, tx)
			if err != nil {
				fmt.Printf("MIGRATION FAILED for migration %v.\n", version)
				fmt.Printf("Error: %v\n", err)
				return
			}

			_, err = tx.Exec(ctx, "UPDATE hmn_migration SET version = $1", previousVersion)
			if err != nil {
				panic(fmt.Errorf("failed to update version in migrations table: %w", err))
			}

			err = tx.Commit(ctx)
			if err != nil {
				panic(fmt.Errorf("failed to commit transaction: %w", err))
			}
		}
	} else {
		fmt.Println("Already migrated; nothing to do.")
	}
}

func Rollback() {
	ctx := context.Background()

	conn := db.NewConnWithConfig(config.PostgresConfig{
		LogLevel: tracelog.LogLevelWarn,
	})
	defer conn.Close(ctx)

	currentVersion := tryGetCurrentVersion(ctx)
	if currentVersion.IsZero() {
		fmt.Println("You have never run migrations; nothing to do.")
		return
	}

	var target types.MigrationVersion
	versions := getSortedMigrationVersions()
	for i := 1; i < len(versions); i++ {
		if versions[i].Equal(currentVersion) {
			target = versions[i-1]
		}
	}

	// NOTE(ben): It occurs to me that we don't have a way to roll back the initial migration, ever.
	// Not that we would ever want to....?

	if target.IsZero() {
		fmt.Println("You are already at the earliest migration; nothing to do.")
		return
	}

	Migrate(target)
}

//go:embed migrationTemplate.txt
var migrationTemplate string

func MakeMigration(name, description string) {
	result := migrationTemplate
	result = strings.ReplaceAll(result, "%NAME%", name)
	result = strings.ReplaceAll(result, "%DESCRIPTION%", fmt.Sprintf("%#v", description))

	now := time.Now().UTC()
	nowConstructor := fmt.Sprintf("time.Date(%d, %d, %d, %d, %d, %d, 0, time.UTC)", now.Year(), now.Month(), now.Day(), now.Hour(), now.Minute(), now.Second())
	result = strings.ReplaceAll(result, "%DATE%", nowConstructor)

	safeVersion := strings.ReplaceAll(types.MigrationVersion(now).String(), ":", "")
	filename := fmt.Sprintf("%v_%v.go", safeVersion, name)
	path := filepath.Join("src", "migration", "migrations", filename)

	err := os.WriteFile(path, []byte(result), 0644)
	if err != nil {
		panic(fmt.Errorf("failed to write migration file: %w", err))
	}

	fmt.Println("Successfully created migration file:")
	fmt.Println(path)
}

func ResetDB() {
	fmt.Println("Resetting database...")

	ctx := context.Background()

	// Create the HMN database user
	credentials := append(
		[]pgCredentials{
			{getSystemUsername(), "", true}, // Postgres.app on Mac
		},
		guessCredentials()...,
	)

	var superuserConn *pgconn.PgConn
	var connErrors []error
	for _, cred := range credentials {
		// NOTE(asaf): We have to use the low-level API of pgconn, because the pgx Exec always wraps the query in a transaction.
		var err error
		superuserConn, err = connectLowLevel(ctx, cred.User, cred.Password)
		if err == nil {
			if cred.SafeToPrint {
				fmt.Printf("Connected by guessing username \"%s\" and password \"%s\".\n", cred.User, cred.Password)
			}
			break
		} else {
			connErrors = append(connErrors, err)
		}
	}
	if superuserConn == nil {
		fmt.Println("Failed to connect to the db to reset it.")
		fmt.Println("The following errors occurred for each attempted set of credentials:")
		for _, err := range connErrors {
			fmt.Printf("- %v\n", err)
		}
		fmt.Println()
		fmt.Println("If this is a local development environment, please let us know what platform you")
		fmt.Println("are using and how you installed Postgres. We want to try and streamline the setup")
		fmt.Println("process for you.")
		fmt.Println()
		fmt.Println("If on the other hand this is a real deployment, please go into psql and manually")
		fmt.Println("create the user:")
		fmt.Println()
		fmt.Println("    CREATE USER <username> WITH")
		fmt.Println("        ENCRYPTED PASSWORD '<password>'")
		fmt.Println("        CREATEDB;")
		fmt.Println()
		fmt.Println("and add the username and password to your config.")
		os.Exit(1)
	}
	defer superuserConn.Close(ctx)

	// Create the HMN user
	{
		result := superuserConn.ExecParams(ctx, fmt.Sprintf(`
				CREATE USER %s WITH
					ENCRYPTED PASSWORD '%s'
					CREATEDB
			`, config.Config.Postgres.User, config.Config.Postgres.Password), nil, nil, nil, nil)
		_, err := result.Close()
		pgErr, isPgError := err.(*pgconn.PgError)
		if err != nil {
			if !(isPgError && pgErr.SQLState() == "42710") { // NOTE(ben): 42710 means "duplicate object", i.e. already exists
				panic(fmt.Errorf("failed to create HMN user: %w", err))
			}
		}
	}

	// Disconnect all other users
	{
		result := superuserConn.ExecParams(ctx, fmt.Sprintf(`
				SELECT pg_terminate_backend(pid)
				FROM pg_stat_activity
				WHERE datname IN ('%s', 'template1') AND pid <> pg_backend_pid()
			`, config.Config.Postgres.DbName), nil, nil, nil, nil)
		_, err := result.Close()
		if err != nil {
			panic(fmt.Errorf("failed to disconnect other users: %w", err))
		}
	}
	superuserConn.Close(ctx)

	// Connect as the HMN user
	conn, err := connectLowLevel(ctx, config.Config.Postgres.User, config.Config.Postgres.Password)
	if err != nil {
		panic(fmt.Errorf("failed to connect to db: %w", err))
	}
	defer conn.Close(ctx)

	// Drop the database
	{
		result := conn.ExecParams(ctx, fmt.Sprintf("DROP DATABASE %s", config.Config.Postgres.DbName), nil, nil, nil, nil)
		_, err := result.Close()
		pgErr, isPgError := err.(*pgconn.PgError)
		if err != nil {
			if !(isPgError && pgErr.SQLState() == "3D000") { // NOTE(asaf): 3D000 means "Database does not exist"
				panic(fmt.Errorf("failed to drop db: %w", err))
			}
		}
	}

	// Create the database again
	{
		result := conn.ExecParams(ctx, fmt.Sprintf("CREATE DATABASE %s", config.Config.Postgres.DbName), nil, nil, nil, nil)
		_, err := result.Close()
		if err != nil {
			panic(fmt.Errorf("failed to create db: %w", err))
		}
	}

	fmt.Println("Database reset successfully.")
}

func connectLowLevel(ctx context.Context, username, password string) (*pgconn.PgConn, error) {
	// NOTE(asaf): We connect to db "template1", because we have to connect to something other than our own db in order to drop it.
	template1DSN := fmt.Sprintf("user=%s password=%s host=%s port=%d dbname=%s",
		username,
		password,
		config.Config.Postgres.Hostname,
		config.Config.Postgres.Port,
		"template1", // NOTE(asaf): template1 must always exist in postgres, as it's the db that gets cloned when you create new DBs
	)
	return pgconn.Connect(ctx, template1DSN)
}

func getSystemUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

type pgCredentials struct {
	User        string
	Password    string
	SafeToPrint bool
}

var commonRootUsernames = []string{getSystemUsername(), "postgres", "root"}
var commonRootPasswords = []string{"", "password", "postgres"}

func guessCredentials() []pgCredentials {
	var result []pgCredentials
	for _, username := range commonRootUsernames {
		for _, password := range commonRootPasswords {
			result = append(result, pgCredentials{username, password, true})
		}
	}
	return result
}
