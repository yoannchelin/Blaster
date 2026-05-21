package diff_test

import (
	"strings"
	"testing"

	"github.com/yourname/blast-radius/internal/diff"
)

// A representative unified diff: two files, one new, one modified with
// multiple hunks. We assert the parser sees what we expect.
const sampleDiff = `diff --git a/internal/payment/charge.go b/internal/payment/charge.go
index 1234567..89abcde 100644
--- a/internal/payment/charge.go
+++ b/internal/payment/charge.go
@@ -10,7 +10,7 @@ package payment
 func ChargeCustomer(p Provider, customerID string, amountCents int) (string, error) {
   if amountCents <= 0 {
-    return "", fmt.Errorf("amount must be positive")
+    return "", fmt.Errorf("amount must be positive, got %d", amountCents)
   }
   return p.Charge(customerID, amountCents)
 }
@@ -42,3 +42,8 @@ func (s *StripeProvider) Refund(txID string) error {
   return nil
 }
+
+// NewStripeProvider constructs a configured StripeProvider.
+func NewStripeProvider(key string) *StripeProvider {
+    return &StripeProvider{APIKey: key}
+}
diff --git a/internal/payment/klarna.go b/internal/payment/klarna.go
new file mode 100644
index 0000000..deadbee
--- /dev/null
+++ b/internal/payment/klarna.go
@@ -0,0 +1,12 @@
+package payment
+
+type KlarnaProvider struct{}
+
+func (k *KlarnaProvider) Charge(c string, a int) (string, error) {
+    return "klarna-" + c, nil
+}
+
+func (k *KlarnaProvider) Refund(t string) error {
+    return nil
+}
`

func TestParse(t *testing.T) {
	files, err := diff.Parse(strings.NewReader(sampleDiff))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// File 1: modified, 2 hunks.
	f1 := files[0]
	if f1.Path != "internal/payment/charge.go" {
		t.Errorf("file 1 path = %q", f1.Path)
	}
	if f1.IsNew {
		t.Error("file 1 marked as new")
	}
	if len(f1.Hunks) != 2 {
		t.Errorf("file 1 hunks = %d, want 2", len(f1.Hunks))
	}
	if f1.Hunks[0].Start != 10 || f1.Hunks[0].Count != 7 {
		t.Errorf("file 1 hunk 1 = %+v", f1.Hunks[0])
	}
	if f1.Hunks[1].Start != 42 || f1.Hunks[1].Count != 8 {
		t.Errorf("file 1 hunk 2 = %+v", f1.Hunks[1])
	}

	// File 2: new, 1 hunk starting at 1.
	f2 := files[1]
	if f2.Path != "internal/payment/klarna.go" {
		t.Errorf("file 2 path = %q", f2.Path)
	}
	if !f2.IsNew {
		t.Error("file 2 should be marked as new")
	}
	if len(f2.Hunks) != 1 {
		t.Fatalf("file 2 hunks = %d, want 1", len(f2.Hunks))
	}
	if f2.Hunks[0].Start != 1 {
		t.Errorf("file 2 hunk start = %d, want 1", f2.Hunks[0].Start)
	}
}

func TestParseEmpty(t *testing.T) {
	files, err := diff.Parse(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parse empty: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

// Single-count hunk header: "@@ -5 +5 @@" without commas.
func TestParseSingleLineHunk(t *testing.T) {
	d := `diff --git a/x.go b/x.go
--- a/x.go
+++ b/x.go
@@ -5 +5 @@
-old line
+new line
`
	files, err := diff.Parse(strings.NewReader(d))
	if err != nil || len(files) != 1 {
		t.Fatalf("parse: %v, %d files", err, len(files))
	}
	if len(files[0].Hunks) != 1 || files[0].Hunks[0].Start != 5 || files[0].Hunks[0].Count != 1 {
		t.Errorf("hunk = %+v", files[0].Hunks)
	}
}
