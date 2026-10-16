package viewsadmin

// Sample data for the WA Relay overview design. These values are static
// placeholders until real metrics (revenue, quotas, volume history, alerts,
// activity feed) are wired to backend queries.

type sampleStat struct {
	Label string
	Value string
	Delta string
	// Tone is "good" or "bad"; controls the delta color.
	Tone string
	// Icon is the key of the stat icon (see statIcon in the view).
	Icon string
}

var sampleStats = []sampleStat{
	{Label: "Messages sent", Value: "48,210", Delta: "+12.4%", Tone: "good", Icon: "send"},
	{Label: "Delivery rate", Value: "97.8%", Delta: "+0.6%", Tone: "good", Icon: "check-circle"},
	{Label: "Failed", Value: "1,061", Delta: "-8.1%", Tone: "good", Icon: "x-circle"},
	{Label: "Active clients", Value: "23", Delta: "+3", Tone: "good", Icon: "users"},
	{Label: "Revenue (30d)", Value: "$4,820", Delta: "+9.2%", Tone: "good", Icon: "revenue"},
}

// sampleVolume are bar heights (percent of chart height) for the message
// volume chart, oldest to newest.
var sampleVolume = []int{36, 47, 43, 59, 68, 55, 75, 82, 66, 73, 91, 84, 78, 100}

type sampleHealthRow struct {
	Label string
	Value string
	// Tone is "success", "warning" or "" (plain strong text).
	Tone string
}

var sampleHealth = []sampleHealthRow{
	{Label: "Quality rating", Value: "Green", Tone: "success"},
	{Label: "Messaging tier", Value: "10K / day"},
	{Label: "Block rate", Value: "1.4%", Tone: "warning"},
	{Label: "Queue", Value: "37 pending"},
}

const sampleHealthNote = "Daily limit used: 48%"

type sampleAlert struct {
	// Severity is "danger" or "warning".
	Severity string
	Text     string
}

var sampleAlerts = []sampleAlert{
	{Severity: "danger", Text: "Block rate for Acme Shop reached 2.1% (limit 2%)"},
	{Severity: "warning", Text: "Template “order_update_v2” still pending approval"},
	{Severity: "warning", Text: "FastCourier used 92% of monthly quota"},
}

type sampleClientRow struct {
	Name      string
	Plan      string
	QuotaPct  int
	Delivery  string
	Status    string
	// StatusTone is "success", "warning" or "danger".
	StatusTone string
}

var sampleClients = []sampleClientRow{
	{Name: "Acme Shop", Plan: "Pro", QuotaPct: 92, Delivery: "94.1%", Status: "Warning", StatusTone: "warning"},
	{Name: "FastCourier", Plan: "Business", QuotaPct: 88, Delivery: "98.9%", Status: "Active", StatusTone: "success"},
	{Name: "ClinicPlus", Plan: "Starter", QuotaPct: 41, Delivery: "99.2%", Status: "Active", StatusTone: "success"},
	{Name: "PayLater", Plan: "Pro", QuotaPct: 64, Delivery: "96.5%", Status: "Active", StatusTone: "success"},
	{Name: "QuickBank", Plan: "Business", QuotaPct: 100, Delivery: "91.0%", Status: "Suspended", StatusTone: "danger"},
}

type sampleFeedItem struct {
	Time    string
	Client  string
	Message string
	// Status is read/delivered/failed/sent; StatusTone is its badge color key.
	Status     string
	StatusTone string
}

var sampleFeed = []sampleFeedItem{
	{Time: "04:47", Client: "FastCourier", Message: "Your parcel is out for delivery", Status: "read", StatusTone: "success"},
	{Time: "04:46", Client: "ClinicPlus", Message: "Appointment reminder – tomorrow 10:00", Status: "delivered", StatusTone: "primary"},
	{Time: "04:46", Client: "Acme Shop", Message: "Order #1842 confirmed", Status: "failed", StatusTone: "danger"},
	{Time: "04:45", Client: "PayLater", Message: "Payment due in 3 days", Status: "sent", StatusTone: "muted"},
}
