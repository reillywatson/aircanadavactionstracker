package main

import (
	"encoding/json"
	"testing"
	"time"
)

const fixture = `{"results":{"packages":[{"id":"eSb2ljEkwe-alTUfKpQ0iU6","information":{"mealPlanName":"All-Inclusive Plan","destination":"PUJ","distanceToAirportKm":25.3,"refundable":true,"transferIncluded":true},"hotelId":"PUJGPC","roomId":"FAMI","selectedOptionIds":{"itineraryId":"J3VNt36ntz"},"pricing":{"perPaxType":[{"paxType":"A","paxCount":2,"wasPrice":2489.00,"price":1732.00,"taxFees":627.00,"total":2359.00},{"paxType":"C","paxCount":1,"paxAge":3,"total":1709.00}],"total":11554.00,"grandTotal":11554.00},"promotions":[{"description":"2000 extra Aeroplan pts"}]}],"metaData":{"totalAvailable":27}},"hotels":{"PUJGPC":{"name":"Bahia Principe Explore Punta Cana","hotelChain":{"name":"Bahia Principe"},"starCategory":4.5,"destination":"Punta Cana","rating":{"value":3.9,"reviews":27016},"rooms":{"FAMI":{"name":"Family Deluxe Suite King"}}}},"itineraries":{"J3VNt36ntz":{"segments":[{"bound":"O","departureTime":"0630","arrivalTime":"1210","flightNumber":"1794","carrierCode":"AC","numConnections":0},{"bound":"I","departureTime":"2240","arrivalTime":"0225","flightNumber":"1799","carrierCode":"AC","numConnections":0}]}}}`

func TestToRows(t *testing.T) {
	var res searchResponse
	if err := json.Unmarshal([]byte(fixture), &res); err != nil {
		t.Fatal(err)
	}
	dep := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	rows := toRows("caribbean", dep, dep.AddDate(0, 0, 7), &res)
	if len(rows) != 1 || len(rows[0]) != len(csvHeader) {
		t.Fatalf("got %v", rows)
	}
	t.Log(rows[0])
}

func TestExtractToken(t *testing.T) {
	ok := `{"error":null,"scope":"IBE_USER","access_token":"a.b.c","expires_in":300,"id_token":"a.b.c","refresh_token":null,"token_type":"bearer"}`
	if tok, err := extractToken([]byte(ok)); err != nil || tok != "a.b.c" {
		t.Errorf("got %q %v", tok, err)
	}
	if _, err := extractToken([]byte(`{"error":"nope","access_token":null}`)); err == nil {
		t.Error("expected error")
	}
}
