package proto

import "testing"

func TestSessionStateKeyIsDurable(t *testing.T) {
	if SessionStateKey("e4dd7710-9dab-449d-b6ec-ab0f74d05c2a") != "agents-api-e4dd7710-9dab-449d-b6ec-ab0f74d05c2a" {
		t.Fatal("native history identity changed")
	}
}
