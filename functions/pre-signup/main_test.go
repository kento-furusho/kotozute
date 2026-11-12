package main

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cognitoidentityprovider/types"
)

func TestSplitExternalUsername(t *testing.T) {
	cases := []struct {
		in, provider, id string
		ok               bool
	}{
		{"google_1234567890", "Google", "1234567890", true},
		{"Google_1234567890", "Google", "1234567890", true},
		{"signinwithapple_001234.abcd.0123", "SignInWithApple", "001234.abcd.0123", true},
		{"facebook_1", "", "", false},
		{"google_", "", "", false},
		{"8f1c2d3e-uuid-native", "", "", false},
	}
	for _, c := range cases {
		p, id, ok := splitExternalUsername(c.in)
		if p != c.provider || id != c.id || ok != c.ok {
			t.Errorf("%q: got (%q,%q,%v) want (%q,%q,%v)", c.in, p, id, ok, c.provider, c.id, c.ok)
		}
	}
}

func TestLinkableEmail(t *testing.T) {
	cases := []struct {
		email, verified string
		want            bool
	}{
		{"a@example.com", "true", true},
		{"a@example.com", "True", true},
		{"a@example.com", "false", false},
		{"a@example.com", "", false},
		{"", "true", false},
		{`a"@example.com`, "true", false},
		{`a\@example.com`, "true", false},
	}
	for _, c := range cases {
		if got := linkableEmail(c.email, c.verified); got != c.want {
			t.Errorf("%q/%q: got %v want %v", c.email, c.verified, got, c.want)
		}
	}
}

func user(name string, status types.UserStatusType, verified string) types.UserType {
	return types.UserType{
		Username:   aws.String(name),
		UserStatus: status,
		Attributes: []types.AttributeType{{Name: aws.String("email_verified"), Value: aws.String(verified)}},
	}
}

func TestPickNativeUser(t *testing.T) {
	t.Run("外部ユーザーと未確認は飛ばす", func(t *testing.T) {
		got, ok := pickNativeUser([]types.UserType{
			user("google_1", types.UserStatusTypeExternalProvider, "true"),
			user("native-unverified", types.UserStatusTypeUnconfirmed, "false"),
			user("native-ok", types.UserStatusTypeConfirmed, "true"),
		})
		if !ok || got != "native-ok" {
			t.Fatalf("got %q,%v", got, ok)
		}
	})
	t.Run("該当なし", func(t *testing.T) {
		if _, ok := pickNativeUser([]types.UserType{user("google_1", types.UserStatusTypeExternalProvider, "true")}); ok {
			t.Fatal("外部ユーザーには紐づけない")
		}
		if _, ok := pickNativeUser(nil); ok {
			t.Fatal("空なら見つからない")
		}
	})
}
