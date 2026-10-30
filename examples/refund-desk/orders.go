package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	kit "github.com/mark3labs/kit/pkg/kit"

	"github.com/mark3labs/bonnie"
)

// order is one order of the sample shop. A real desk reads its orders from
// the shop's database; this one has a fixed table, so an eval knows what
// each order must give.
type order struct {
	ID      string  `json:"id"`
	Item    string  `json:"item"`
	Total   float64 `json:"total"`
	Status  string  `json:"status"`
	Days    int     `json:"days_since_delivery,omitempty"`
	Comment string  `json:"comment,omitempty"`
}

// orders is the sample shop. Days counts from delivery, not from a date, so
// the evals give the same result on each day.
var orders = map[string]order{
	"A-1001": {ID: "A-1001", Item: "Wireless headphones", Total: 59, Status: "delivered", Days: 10},
	"A-1002": {ID: "A-1002", Item: "Standing desk", Total: 249, Status: "delivered", Days: 5},
	"A-1003": {ID: "A-1003", Item: "Mechanical keyboard", Total: 89, Status: "delivered", Days: 45},
	"A-1004": {ID: "A-1004", Item: "Webcam", Total: 39, Status: "shipped", Comment: "in transit, not delivered yet"},
}

const (
	// refundWindow is the number of days after delivery that a refund is
	// allowed.
	refundWindow = 30
	// refundCap is the largest refund that issue_refund makes. A larger
	// one goes to a person. The approval limit in instructions.md is lower:
	// the prompt asks for approval, and this code is the hard limit.
	refundCap = 500
)

// refund is what issue_refund returns to the model.
type refund struct {
	RefundID string  `json:"refund_id"`
	OrderID  string  `json:"order_id"`
	Amount   float64 `json:"amount"`
}

// orderTools builds lookup_order and issue_refund for one run.
func orderTools(_ context.Context, scope bonnie.ToolScope) ([]kit.Tool, error) {
	// lookup_order only reads, so a call that a crash interrupted can run
	// again. issue_refund is not replay-safe: it has an effect.
	scope.Session.AddReplaySafeTools("lookup_order")

	type lookupInput struct {
		OrderID string `json:"order_id" description:"The order ID, for example A-1001."`
	}
	lookup := kit.NewTool("lookup_order",
		"Look up one order: its item, total, status, and the days since delivery.",
		func(_ context.Context, in lookupInput) (kit.ToolOutput, error) {
			o, ok := orders[normalize(in.OrderID)]
			if !ok {
				return kit.ErrorResult(fmt.Sprintf("no order %q", in.OrderID)), nil
			}
			return jsonResult(o)
		})

	// refunded is closure state, thus it is not durable: it only stops a
	// second refund in one process. A real desk gives the payment provider
	// the refund ID as an idempotency key, so a refund that a crash repeats
	// is made once.
	var mu sync.Mutex
	refunded := map[string]bool{}

	type refundInput struct {
		OrderID string  `json:"order_id" description:"The order ID, for example A-1001."`
		Amount  float64 `json:"amount" description:"The amount to refund, in dollars."`
		Reason  string  `json:"reason" description:"Why the customer wants the refund."`
	}
	issue := kit.NewTool("issue_refund",
		"Refund an order to the customer's original payment method.",
		func(_ context.Context, in refundInput) (kit.ToolOutput, error) {
			o, ok := orders[normalize(in.OrderID)]
			switch {
			case !ok:
				return kit.ErrorResult(fmt.Sprintf("no order %q", in.OrderID)), nil
			case o.Status != "delivered":
				return kit.ErrorResult(o.ID + " is not delivered; it cannot be refunded yet"), nil
			case o.Days > refundWindow:
				return kit.ErrorResult(fmt.Sprintf("%s was delivered %d days ago; refunds are allowed for %d days", o.ID, o.Days, refundWindow)), nil
			case in.Amount <= 0 || in.Amount > o.Total:
				return kit.ErrorResult(fmt.Sprintf("the amount must be more than 0 and at most the order total, %.2f", o.Total)), nil
			case in.Amount > refundCap:
				return kit.ErrorResult(fmt.Sprintf("refunds above %d dollars go to a person", refundCap)), nil
			}
			mu.Lock()
			defer mu.Unlock()
			if refunded[o.ID] {
				return kit.ErrorResult(o.ID + " is already refunded"), nil
			}
			refunded[o.ID] = true
			return jsonResult(refund{RefundID: "R-" + o.ID, OrderID: o.ID, Amount: in.Amount})
		})
	return []kit.Tool{lookup, issue}, nil
}

func normalize(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
}

func jsonResult(v any) (kit.ToolOutput, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return kit.ToolOutput{}, err
	}
	return kit.TextResult(string(out)), nil
}
