package outletpolicy

// OrderWorkflow is what happens to an order the moment it is placed (or released after online
// acceptance). It is decided once, from the outlet's use case and the order subtype, and every
// path that creates or opens an order (till sale, online ingestion, held-order release, items added
// to a draft) follows it, so the workflows stay separate per use case without each caller
// re-deriving them:
//
//   - hospitality and quick service: prepared orders (dine-in, takeaway, delivery, room service,
//     bar tab) open at once and go to the kitchen: KDS tickets on the routed stations plus kitchen
//     and bar chits.
//   - retail, and goods sold by a services outlet: a takeaway (collect) or delivery order opens at
//     once into the pickup or delivery queue as a pick list; nothing goes to a kitchen. A plain
//     counter sale stays a draft until it is paid.
//   - services job orders (printing, garage, laundry): open at once onto the production board.
//     Online service bookings never reach here; they become appointments.
type OrderWorkflow struct {
	// OpenOnCreate commits the order at once ("open") instead of leaving it a draft until paid.
	OpenOnCreate bool
	// Kitchen sends the order to the outlet's kitchen/bar stations (KDS tickets and chits).
	Kitchen bool
	// Production puts a services job on the production board (KDS tickets, no chits).
	Production bool
	// Queue is the counter queue the order is handed over from: "pickup", "delivery" or "".
	Queue string
}

// Order queues.
const (
	QueuePickup   = "pickup"
	QueueDelivery = "delivery"
)

// KitchenUseCase reports whether the use case prepares food and drink to order.
func KitchenUseCase(useCase string) bool {
	switch NormalizeUseCase(useCase) {
	case UseCaseHospitality, UseCaseQuickService:
		return true
	}
	return false
}

// DefaultOrderSubtype is the subtype an order gets when the client sends none: a table order in a
// kitchen outlet, a counter sale everywhere else.
func DefaultOrderSubtype(useCase string) string {
	if KitchenUseCase(useCase) {
		return "dine_in"
	}
	return "retail"
}

// WorkflowFor returns the workflow for an order subtype at an outlet of the given use case.
func WorkflowFor(useCase, subtype string) OrderWorkflow {
	queue := ""
	switch subtype {
	case "takeaway":
		queue = QueuePickup
	case "delivery":
		queue = QueueDelivery
	}
	if subtype == "service_job" {
		return OrderWorkflow{OpenOnCreate: true, Production: true}
	}
	if KitchenUseCase(useCase) {
		switch subtype {
		case "dine_in", "takeaway", "delivery", "room_service", "bar_tab":
			return OrderWorkflow{OpenOnCreate: true, Kitchen: true, Queue: queue}
		}
		return OrderWorkflow{}
	}
	// Retail and services goods: collect and delivery orders go straight to their queue.
	if queue != "" {
		return OrderWorkflow{OpenOnCreate: true, Queue: queue}
	}
	return OrderWorkflow{}
}
