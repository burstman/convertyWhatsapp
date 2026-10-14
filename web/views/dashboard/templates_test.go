package viewsdashboard

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"whatsappconverty/web/views/components"
)

// The templates page must refresh via a POST form (which the CSRF middleware
// guards) and must not expose a GET link to /templates/refresh.
func TestTemplatesRefreshIsPostForm(t *testing.T) {
	var buf bytes.Buffer
	page := TemplatesPage(components.Page{Title: "Templates"}, nil, TemplateFlash{})
	if err := page.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := buf.String()

	if !strings.Contains(html, `method="post"`) {
		t.Fatalf("templates refresh control has no POST method")
	}
	if !strings.Contains(html, `action="/templates/refresh"`) {
		t.Fatalf("templates refresh form does not post to /templates/refresh")
	}
	if strings.Contains(html, `href="/templates/refresh"`) {
		t.Fatalf("templates page still exposes a GET link to /templates/refresh")
	}
}
