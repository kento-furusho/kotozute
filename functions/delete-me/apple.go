package main

// Sign in with Apple の連携解除（App Store 審査ガイドライン 5.1.1(v)）。
//
// Apple でログインした人がアカウントを消すときは、Apple 側の「このアプリを使っている」も
// 取り消す必要がある。取り消しには Apple が発行したトークンが要るが、
// ログインは Cognito が仲介していて、Apple のトークンは Cognito の中にしかない。
//
// そこで削除の直前に、アプリで Apple の本人確認をもう一度してもらい、
// 使い捨ての authorization code を受け取る。サーバーはそれを
//   1. /auth/token でリフレッシュトークンに引き換え
//   2. /auth/revoke で取り消す
// 鍵は Cognito の Apple 連携と同じもの（Secrets Manager の kotozute/social-login）。

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	cognito "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

const (
	appleTeamID = "5V6S74G5N9"
	// アプリ内の Apple ログイン（ASAuthorization）が発行する code は、
	// Services ID ではなくアプリのバンドル ID 宛て。引き換えも同じ client_id で行う。
	appleClientID   = "dev.kotozute.app"
	appleBaseURL    = "https://appleid.apple.com"
	appleSecretName = "kotozute/social-login"
	// Cognito の Apple 連携の名前（template.yaml の ProviderName）。
	appleProviderName = "SignInWithApple"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

// appleLinked は、この Cognito ユーザーに Apple のログインが紐づいているかを返す。
//
// トークンの中身ではなく Cognito に聞く。メールで登録したあと Apple を紐づけた人は、
// メールでログインしていてもトークンに Apple の印が無いため。
func appleLinked(ctx context.Context, username string) (bool, error) {
	if userPoolID == "" {
		return false, nil // ローカル実行（Cognito なし）
	}
	out, err := cognitoClient.AdminGetUser(ctx, &cognito.AdminGetUserInput{
		UserPoolId: aws.String(userPoolID),
		Username:   aws.String(username),
	})
	if err != nil {
		return false, err
	}
	for _, attr := range out.UserAttributes {
		if aws.ToString(attr.Name) == "identities" {
			return identitiesIncludeApple(aws.ToString(attr.Value)), nil
		}
	}
	return false, nil
}

// identitiesIncludeApple は Cognito の identities 属性（JSON の配列）に Apple があるかを見る。
func identitiesIncludeApple(raw string) bool {
	var identities []struct {
		ProviderName string `json:"providerName"`
	}
	if err := json.Unmarshal([]byte(raw), &identities); err != nil {
		return false
	}
	for _, id := range identities {
		if id.ProviderName == appleProviderName {
			return true
		}
	}
	return false
}

type appleKey struct {
	KeyID      string `json:"appleKeyId"`
	PrivateKey string `json:"applePrivateKey"`
}

func loadAppleKey(ctx context.Context) (appleKey, error) {
	out, err := secretsClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(appleSecretName),
	})
	if err != nil {
		return appleKey{}, err
	}
	var key appleKey
	if err := json.Unmarshal([]byte(aws.ToString(out.SecretString)), &key); err != nil {
		return appleKey{}, err
	}
	if key.KeyID == "" || key.PrivateKey == "" {
		return appleKey{}, errors.New("apple key is missing in the secret")
	}
	return key, nil
}

// appleClientSecret は Apple の API に渡す client_secret（ES256 の JWT）を作る。
// 有効期限は短くてよい（その場で2回使うだけ）。
func appleClientSecret(key appleKey, now time.Time) (string, error) {
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return "", errors.New("apple private key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	priv, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return "", errors.New("apple private key is not ECDSA")
	}

	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": key.KeyID})
	claims, _ := json.Marshal(map[string]any{
		"iss": appleTeamID,
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"aud": appleBaseURL,
		"sub": appleClientID,
	})
	signingInput := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, priv, digest[:])
	if err != nil {
		return "", err
	}
	// JWT の ES256 は DER ではなく r||s（各32バイト）。
	sig := append(pad32(r), pad32(s)...)
	return signingInput + "." + enc.EncodeToString(sig), nil
}

func pad32(n *big.Int) []byte {
	b := n.Bytes()
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// revokeApple は authorization code を引き換えて、そのトークンを取り消す。
func revokeApple(ctx context.Context, code string) error {
	key, err := loadAppleKey(ctx)
	if err != nil {
		return fmt.Errorf("load key: %w", err)
	}
	secret, err := appleClientSecret(key, time.Now())
	if err != nil {
		return fmt.Errorf("client secret: %w", err)
	}

	body, err := applePost(ctx, "/auth/token", url.Values{
		"client_id":     {appleClientID},
		"client_secret": {secret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	var tokens struct {
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokens); err != nil {
		return fmt.Errorf("token response: %w", err)
	}
	token, hint := tokens.RefreshToken, "refresh_token"
	if token == "" {
		token, hint = tokens.AccessToken, "access_token"
	}
	if token == "" {
		return errors.New("token response has no token")
	}

	_, err = applePost(ctx, "/auth/revoke", url.Values{
		"client_id":       {appleClientID},
		"client_secret":   {secret},
		"token":           {token},
		"token_type_hint": {hint},
	})
	if err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	return nil
}

func applePost(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, appleBaseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		// 本文は {"error":"invalid_grant"} 程度。トークンは含まれない。
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}
