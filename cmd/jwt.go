package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/murdinc/springg/internal/springg"
	"github.com/spf13/cobra"
)

var (
	jwtUserID  string
	jwtClaims  string
	jwtVerbose bool
)

var generateJWTCmd = &cobra.Command{
	Use:   "generate-jwt",
	Short: "Generate a JWT token for authentication",
	Long: `Generate a JWT token that never expires for use with the Springg API.
The token is signed with the jwt_secret from the configuration file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Load configuration
		config, err := springg.LoadConfig()
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}

		if config.JWTSecret == "" {
			return fmt.Errorf("jwt_secret not configured in springg.json")
		}

		// Parse additional claims if provided
		customClaims := make(map[string]interface{})
		if jwtClaims != "" {
			if err := json.Unmarshal([]byte(jwtClaims), &customClaims); err != nil {
				return fmt.Errorf("invalid JSON in --claims: %w", err)
			}
		}

		// Set default user ID
		if jwtUserID == "" {
			jwtUserID = "springg-client"
		}

		// Set default permissions if not provided in custom claims
		if _, hasPermissions := customClaims["permissions"]; !hasPermissions {
			customClaims["permissions"] = []string{"read", "write"}
		}

		// Create JWT claims
		claims := jwt.MapClaims{
			"iss":     "springg",
			"iat":     time.Now().Unix(),
			"user_id": jwtUserID,
		}

		// Add custom claims
		for key, value := range customClaims {
			claims[key] = value
		}

		// Create token
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

		// Sign token
		tokenString, err := token.SignedString([]byte(config.JWTSecret))
		if err != nil {
			return fmt.Errorf("failed to sign token: %w", err)
		}

		// Output token
		if jwtVerbose {
			fmt.Println("Generated JWT Token:")
			fmt.Println("-------------------")
			fmt.Println(tokenString)
			fmt.Println()
			fmt.Println("Token Details:")
			fmt.Printf("  User ID:     %s\n", jwtUserID)
			fmt.Printf("  Issuer:      springg\n")
			fmt.Printf("  Issued At:   %s\n", time.Now().Format(time.RFC3339))
			fmt.Printf("  Expires:     Never\n")
			if len(customClaims) > 0 {
				fmt.Println("  Custom Claims:")
				for key, value := range customClaims {
					fmt.Printf("    %s: %v\n", key, value)
				}
			}
		} else {
			fmt.Println(tokenString)
		}

		return nil
	},
}

func init() {
	generateJWTCmd.Flags().StringVar(&jwtUserID, "user-id", "", "User identifier for the token (default: springg-client)")
	generateJWTCmd.Flags().StringVar(&jwtClaims, "claims", "", "Additional JSON claims to include")
	generateJWTCmd.Flags().BoolVar(&jwtVerbose, "verbose", false, "Show token details")
}
