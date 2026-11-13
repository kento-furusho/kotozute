package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	cognito "github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

var cognitoClient *cognito.Client

func init() {
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		panic(fmt.Sprintf("failed to load AWS config: %v", err))
	}
	cognitoClient = cognito.NewFromConfig(cfg)
}

// handler は Cognito の「登録の前」トリガー。
//
// Apple / Google で**初めて**ログインした人のメールが、すでにメール＋パスワードで登録されている
// アカウントと同じなら、そのアカウントに紐づける（AdminLinkProviderForUser）。
// 紐づけないと別のアカウントができ、ログイン方法を変えた人が空っぽのアカウントに入って
// 「ペアもことづても消えた」ように見える。
//
// **メールが確認済みのときだけ紐づける。** 確認されていないメールで紐づけると、
// 他人のメールを名乗った外部アカウントが、その人のアカウントを乗っ取れてしまう。
// 紐づけ先（メール登録の側）も確認済みであることを求める。
//
// Cognito の仕様で、紐づけた直後のこのログインだけは失敗として返る（"Already found an entry for username"）。
// アプリは「アカウントをまとめました。もう一度お試しください」と出し、次のログインからは
// 紐づけた先のアカウント（同じ sub）に入る。
func handler(ctx context.Context, event events.CognitoEventUserPoolsPreSignup) (events.CognitoEventUserPoolsPreSignup, error) {
	if event.TriggerSource != "PreSignUp_ExternalProvider" {
		return event, nil
	}
	provider, providerUserID, ok := splitExternalUsername(event.UserName)
	if !ok {
		log.Printf("unexpected external username format")
		return event, nil
	}
	email := event.Request.UserAttributes["email"]
	if !linkableEmail(email, event.Request.UserAttributes["email_verified"]) {
		return event, nil
	}

	out, err := cognitoClient.ListUsers(ctx, &cognito.ListUsersInput{
		UserPoolId: aws.String(event.UserPoolID),
		Filter:     aws.String(fmt.Sprintf("email = %q", email)),
	})
	if err != nil {
		// 探せなかったら紐づけずに通す（別アカウントになるが、ログインはできる）。
		log.Printf("list users failed: %v", err)
		return event, nil
	}
	native, found := pickNativeUser(out.Users)
	if !found {
		return event, nil
	}

	if _, err := cognitoClient.AdminLinkProviderForUser(ctx, &cognito.AdminLinkProviderForUserInput{
		UserPoolId: aws.String(event.UserPoolID),
		DestinationUser: &types.ProviderUserIdentifierType{
			ProviderName:           aws.String("Cognito"),
			ProviderAttributeValue: aws.String(native),
		},
		SourceUser: &types.ProviderUserIdentifierType{
			ProviderName:           aws.String(provider),
			ProviderAttributeName:  aws.String("Cognito_Subject"),
			ProviderAttributeValue: aws.String(providerUserID),
		},
	}); err != nil {
		log.Printf("link provider failed (%s): %v", provider, err)
		return event, nil
	}
	log.Printf("linked %s identity to existing user", provider)
	return event, nil
}

// externalProviders は Cognito が外部ユーザーの username に付ける接頭辞（小文字）と、
// AdminLinkProviderForUser に渡すプロバイダ名の対応。
var externalProviders = map[string]string{
	"google":          "Google",
	"signinwithapple": "SignInWithApple",
}

// splitExternalUsername は "google_1234" のような外部ユーザーの username を
// プロバイダ名と、そのプロバイダでの ID に分ける。
func splitExternalUsername(username string) (provider, providerUserID string, ok bool) {
	prefix, id, found := strings.Cut(username, "_")
	if !found || id == "" {
		return "", "", false
	}
	provider, ok = externalProviders[strings.ToLower(prefix)]
	if !ok {
		return "", "", false
	}
	return provider, id, true
}

// linkableEmail は、紐づけに使ってよいメールかを返す。確認済みで、フィルタ式を壊す文字を含まないもの。
func linkableEmail(email, verified string) bool {
	if email == "" || !strings.EqualFold(verified, "true") {
		return false
	}
	return !strings.ContainsAny(email, "\"\\")
}

// pickNativeUser は、メール＋パスワードで登録された（外部ではない）確認済みのユーザーを探して username を返す。
func pickNativeUser(users []types.UserType) (string, bool) {
	for _, u := range users {
		if u.UserStatus == types.UserStatusTypeExternalProvider {
			continue
		}
		if u.Username == nil || !attributeIsTrue(u.Attributes, "email_verified") {
			continue
		}
		return *u.Username, true
	}
	return "", false
}

func attributeIsTrue(attrs []types.AttributeType, name string) bool {
	for _, a := range attrs {
		if a.Name != nil && *a.Name == name && a.Value != nil {
			return strings.EqualFold(*a.Value, "true")
		}
	}
	return false
}

func main() {
	lambda.Start(handler)
}
