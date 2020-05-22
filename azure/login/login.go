package login

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/docker/api/errdefs"

	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/adal"
	"github.com/Azure/go-autorest/autorest/azure/cli"
	"github.com/Azure/go-autorest/autorest/date"
	"golang.org/x/oauth2"

	"github.com/pkg/errors"
)

//go login process, derived from code sample provided by MS at https://github.com/devigned/go-az-cli-stuff
const (
	authorizeFormat  = "https://login.microsoftonline.com/organizations/oauth2/v2.0/authorize?response_type=code&client_id=%s&redirect_uri=%s&state=%s&prompt=select_account&response_mode=query&scope=%s"
	tokenEndpoint    = "https://login.microsoftonline.com/%s/oauth2/v2.0/token"
	authorizationURL = "https://management.azure.com/tenants?api-version=2019-11-01"
	// scopes for a multi-tenant app works for openid, email, other common scopes, but fails when trying to add a token
	// v1 scope like "https://management.azure.com/.default" for ARM access
	scopes   = "offline_access https://management.azure.com/.default"
	clientID = "04b07795-8ddb-461a-bbee-02f9e1bf7b46" // Azure CLI client id
)

type (
	azureToken struct {
		Type         string `json:"token_type"`
		Scope        string `json:"scope"`
		ExpiresIn    int    `json:"expires_in"`
		ExtExpiresIn int    `json:"ext_expires_in"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Foci         string `json:"foci"`
	}

	tenantResult struct {
		Value []tenantValue `json:"value"`
	}
	tenantValue struct {
		TenantID string `json:"tenantId"`
	}
)

// AzureLoginService Service to log into azure and get authentifier for azure APIs
type AzureLoginService struct {
	tokenStore tokenStore
	apiHelper  apiHelper
}

const tokenStoreFilename = "dockerAccessToken.json"

// NewAzureLoginService creates a NewAzureLoginService
func NewAzureLoginService() (AzureLoginService, error) {
	return newAzureLoginServiceFromPath(getTokenStorePath(), azureAPIHelper{})
}

func newAzureLoginServiceFromPath(tokenStorePath string, helper apiHelper) (AzureLoginService, error) {
	store, err := newTokenStore(tokenStorePath)
	if err != nil {
		return AzureLoginService{}, err
	}
	return AzureLoginService{
		tokenStore: store,
		apiHelper:  helper,
	}, nil
}

//Login perform azure login through browser
func (login AzureLoginService) Login(ctx context.Context) error {
	queryCh := make(chan url.Values, 1)
	serverPort, err := startLoginServer(queryCh)
	if err != nil {
		return err
	}

	redirectURL := "http://localhost:" + strconv.Itoa(serverPort)
	login.apiHelper.openAzureLoginPage(redirectURL)

	select {
	case <-ctx.Done():
		return nil
	case qsValues := <-queryCh:
		errorMsg, hasError := qsValues["error"]
		if hasError {
			return fmt.Errorf("login failed : %s", errorMsg)
		}
		code, hasCode := qsValues["code"]
		if !hasCode {
			return errdefs.ErrLoginFailed
		}
		data := url.Values{
			"grant_type":   []string{"authorization_code"},
			"client_id":    []string{clientID},
			"code":         code,
			"scope":        []string{scopes},
			"redirect_uri": []string{redirectURL},
		}
		token, err := login.apiHelper.queryToken(data, "organizations")
		if err != nil {
			return errors.Wrap(err, "Access token request failed")
		}

		bits, statusCode, err := login.apiHelper.queryAuthorizationAPI(authorizationURL, fmt.Sprintf("Bearer %s", token.AccessToken))
		if err != nil {
			return errors.Wrap(err, "login failed")
		}

		if statusCode == 200 {
			var tenantResult tenantResult
			if err := json.Unmarshal(bits, &tenantResult); err != nil {
				return errors.Wrap(err, "login failed")
			}
			tenantID := tenantResult.Value[0].TenantID
			tenantToken, err := login.refreshToken(token.RefreshToken, tenantID)
			if err != nil {
				return errors.Wrap(err, "login failed")
			}
			loginInfo := TokenInfo{TenantID: tenantID, Token: tenantToken}

			err = login.tokenStore.writeLoginInfo(loginInfo)

			if err != nil {
				return errors.Wrap(err, "login failed")
			}
			fmt.Println("Login Succeeded")

			return nil
		}

		return fmt.Errorf("login failed : " + string(bits))
	}
}

func getTokenStorePath() string {
	cliPath, _ := cli.AccessTokensPath()
	return filepath.Join(filepath.Dir(cliPath), tokenStoreFilename)
}

func toOAuthToken(token azureToken) oauth2.Token {
	expireTime := time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	oauthToken := oauth2.Token{
		RefreshToken: token.RefreshToken,
		AccessToken:  token.AccessToken,
		Expiry:       expireTime,
		TokenType:    token.Type,
	}
	return oauthToken
}

// NewAuthorizerFromLogin creates an authorizer based on login access token
func NewAuthorizerFromLogin() (autorest.Authorizer, error) {
	login, err := NewAzureLoginService()
	if err != nil {
		return nil, err
	}
	oauthToken, err := login.GetValidToken()
	if err != nil {
		return nil, err
	}

	token := adal.Token{
		AccessToken:  oauthToken.AccessToken,
		Type:         oauthToken.TokenType,
		ExpiresIn:    json.Number(strconv.Itoa(int(time.Until(oauthToken.Expiry).Seconds()))),
		ExpiresOn:    json.Number(strconv.Itoa(int(oauthToken.Expiry.Sub(date.UnixEpoch()).Seconds()))),
		RefreshToken: "",
		Resource:     "",
	}

	return autorest.NewBearerAuthorizer(&token), nil
}

// GetValidToken returns an access token. Refresh token if needed
func (login AzureLoginService) GetValidToken() (oauth2.Token, error) {
	loginInfo, err := login.tokenStore.readToken()
	if err != nil {
		return oauth2.Token{}, err
	}
	token := loginInfo.Token
	if token.Valid() {
		return token, nil
	}
	tenantID := loginInfo.TenantID
	token, err = login.refreshToken(token.RefreshToken, tenantID)
	if err != nil {
		return oauth2.Token{}, errors.Wrap(err, "access token request failed. Maybe you need to login to azure again.")
	}
	err = login.tokenStore.writeLoginInfo(TokenInfo{TenantID: tenantID, Token: token})
	if err != nil {
		return oauth2.Token{}, err
	}
	return token, nil
}

func (login AzureLoginService) refreshToken(currentRefreshToken string, tenantID string) (oauth2.Token, error) {
	data := url.Values{
		"grant_type":    []string{"refresh_token"},
		"client_id":     []string{clientID},
		"scope":         []string{scopes},
		"refresh_token": []string{currentRefreshToken},
	}
	token, err := login.apiHelper.queryToken(data, tenantID)
	if err != nil {
		return oauth2.Token{}, err
	}

	return toOAuthToken(token), nil
}

func openbrowser(url string) {
	var err error

	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		log.Fatal(err)
	}
}