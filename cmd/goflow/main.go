package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/work-frame/goflow/internal/job"
)

func main() {
	j, err := job.New("send welcome email")
	if err != nil {
		log.Fatal(err)
	}

	out, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(string(out))
}
