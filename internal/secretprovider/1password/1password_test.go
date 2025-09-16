package onepassword

// TODO: Implement tests for the 1Password secret provider. Currently I have no account available for testing.

// TestProvider_GetSecrets_LongLifetime tests the GetSecrets method over an extended period to check if the client
// is able to retrieve secrets without re-initialization even after a while.
// func TestProvider_GetSecrets_LongLifetime(t *testing.T) {
//	ctx := t.Context()
//	startTime := time.Now()
//
//	appConfig, err := config.GetAppConfig()
//	if err != nil {
//		t.Fatalf("unable to get app config: %v", err)
//	}
//
//	if appConfig.SecretProvider != Name {
//		t.Skip("Skipping test as the secret provider is not set to ", Name)
//	}
//
//	cfg, err := GetConfig()
//	if err != nil {
//		t.Fatalf("unable to get config: %v", err)
//	}
//
//	provider, err := NewProvider(ctx, cfg.AccessToken, "test")
//	if err != nil {
//		t.Fatalf("Failed to create Bitwarden provider: %v", err)
//	}
//
//	t.Cleanup(func() {
//		provider.Close()
//	})
//
//	maxTries := 30
//	waitBetweenTries := 10 * time.Second
//
//	for i := 0; i < maxTries; i++ {
//		_, err = provider.GetSecrets(ctx, []string{"op://Doco-CD/Secret Test/OTHER_SECRET"})
//		if err != nil {
//			t.Fatalf("Failed to get secrets: %s", err)
//		}
//
//		t.Logf("Successfully retrieved secrets on attempt %d at elapsed time %v", i+1, time.Since(startTime))
//
//		if i < maxTries-1 {
//			time.Sleep(waitBetweenTries)
//		}
//	}
//}
