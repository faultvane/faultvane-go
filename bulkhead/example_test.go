package bulkhead_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/faultvane/faultvane-go"
	"github.com/faultvane/faultvane-go/bulkhead"
)

func chargeCard(context.Context) error { return nil }

func Example() {
	payments, err := bulkhead.New(
		bulkhead.WithMaxConcurrent(20),
		bulkhead.WithMaxWaiting(10),
		bulkhead.WithMaxWait(50*time.Millisecond),
	)
	if err != nil {
		panic(err)
	}

	err = payments.Do(context.Background(), chargeCard)
	switch {
	case errors.Is(err, bulkhead.ErrFull):
		fmt.Println("the payment service has no free slot")
	case errors.Is(err, faultvane.ErrRejected):
		fmt.Println("a policy rejected the call")
	case err != nil:
		fmt.Println("the call failed:", err)
	}
}

func ExampleBulkhead_InFlight() {
	payments, err := bulkhead.New()
	if err != nil {
		panic(err)
	}
	fmt.Println(payments.InFlight(), payments.Waiting())
}
