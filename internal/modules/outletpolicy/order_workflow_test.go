package outletpolicy

import "testing"

func TestWorkflowFor(t *testing.T) {
	cases := []struct {
		useCase, subtype string
		want             OrderWorkflow
	}{
		// Kitchen outlets: prepared orders open and go to the kitchen.
		{"hospitality", "dine_in", OrderWorkflow{OpenOnCreate: true, Kitchen: true}},
		{"restaurant", "takeaway", OrderWorkflow{OpenOnCreate: true, Kitchen: true, Queue: QueuePickup}},
		{"cafe", "delivery", OrderWorkflow{OpenOnCreate: true, Kitchen: true, Queue: QueueDelivery}},
		{"hotel", "room_service", OrderWorkflow{OpenOnCreate: true, Kitchen: true}},
		{"bar", "bar_tab", OrderWorkflow{OpenOnCreate: true, Kitchen: true}},
		{"quick_service", "takeaway", OrderWorkflow{OpenOnCreate: true, Kitchen: true, Queue: QueuePickup}},
		{"hospitality", "retail", OrderWorkflow{}},
		// Retail: collect and delivery go to their queue, never a kitchen; counter sales stay drafts.
		{"retail", "takeaway", OrderWorkflow{OpenOnCreate: true, Queue: QueuePickup}},
		{"retail", "delivery", OrderWorkflow{OpenOnCreate: true, Queue: QueueDelivery}},
		{"retail", "retail", OrderWorkflow{}},
		{"retail", "dine_in", OrderWorkflow{}},
		{"", "takeaway", OrderWorkflow{OpenOnCreate: true, Queue: QueuePickup}},
		// Services: goods behave like retail; jobs go to the production board.
		{"services", "takeaway", OrderWorkflow{OpenOnCreate: true, Queue: QueuePickup}},
		{"salon", "delivery", OrderWorkflow{OpenOnCreate: true, Queue: QueueDelivery}},
		{"services", "service_job", OrderWorkflow{OpenOnCreate: true, Production: true}},
		{"services", "retail", OrderWorkflow{}},
	}
	for _, c := range cases {
		if got := WorkflowFor(c.useCase, c.subtype); got != c.want {
			t.Errorf("WorkflowFor(%q, %q) = %+v, want %+v", c.useCase, c.subtype, got, c.want)
		}
	}
}

func TestDefaultOrderSubtype(t *testing.T) {
	for uc, want := range map[string]string{"hospitality": "dine_in", "quick_service": "dine_in", "retail": "retail", "services": "retail", "": "retail"} {
		if got := DefaultOrderSubtype(uc); got != want {
			t.Errorf("DefaultOrderSubtype(%q) = %q, want %q", uc, got, want)
		}
	}
}
