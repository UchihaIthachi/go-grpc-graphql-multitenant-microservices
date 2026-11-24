#!/usr/bin/env bash
# End-to-end check against the running compose stack (`make up` first).
# Signs up a shop, adds products, registers a customer, places orders that succeed and fail,
# checks isolation between shops, and checks the events reached Kafka.
set -euo pipefail
GQL=${GQL:-http://localhost:${GATEWAY_PORT:-8080}/graphql}
COMPOSE=${COMPOSE:-docker compose -f deploy/compose/docker-compose.yml}
RUN=$(date +%s)
pass=0; fail=0
ok()  { echo "  PASS  $1"; pass=$((pass+1)); }
bad() { echo "  FAIL  $1"; echo "        $2"; fail=$((fail+1)); }
need() { command -v "$1" >/dev/null || { echo "needs $1"; exit 2; }; }
need curl; need jq

# gql TOKEN QUERY [VARIABLES_JSON] -> response JSON
gql() {
  local auth=() vars=${3:-}
  [ -n "$1" ] && auth=(-H "Authorization: Bearer $1")
  [ -n "$vars" ] || vars='{}'
  jq -n --arg q "$2" --argjson v "$vars" '{query:$q, variables:$v}' |
    curl -sS -X POST "$GQL" -H 'Content-Type: application/json' --data-binary @- ${auth[@]+"${auth[@]}"}
}
check() { # NAME JSON JQ_EXPR
  if echo "$2" | jq -e "$3" >/dev/null 2>&1; then ok "$1"; else bad "$1" "$(echo "$2" | head -c 400)"; fi
}

echo "waiting for the gateway"
for _ in $(seq 60); do curl -sf "${GQL%/graphql}/healthz" >/dev/null && break; sleep 1; done

SIGNUP='mutation($i: SignUpInput!) { signUp(input: $i) { token user { role } } }'
REGISTER='mutation($i: CredentialsInput!) { register(input: $i) { token } }'
CREATE='mutation($i: CreateProductInput!) { createProduct(input: $i) { id } }'
PLACE='mutation($i: PlaceOrderInput!) { placeOrder(input: $i) { id status reason history total { amount currency } } }'

echo "shops and users"
r=$(gql "" "$SIGNUP" "{\"i\":{\"shopName\":\"Acme\",\"shopSlug\":\"acme-$RUN\",\"email\":\"owner@acme.test\",\"password\":\"correct horse battery\"}}")
check "sign up shop acme" "$r" '.data.signUp.user.role == "admin"'
ACME=$(echo "$r" | jq -r .data.signUp.token)
r=$(gql "" "$SIGNUP" "{\"i\":{\"shopName\":\"Globex\",\"shopSlug\":\"globex-$RUN\",\"email\":\"owner@globex.test\",\"password\":\"correct horse battery\"}}")
GLOBEX=$(echo "$r" | jq -r .data.signUp.token)
r=$(gql "" "$REGISTER" "{\"i\":{\"shopSlug\":\"acme-$RUN\",\"email\":\"alice@example.com\",\"password\":\"alice password\"}}")
check "register customer" "$r" '.data.register.token | length > 20'
ALICE=$(echo "$r" | jq -r .data.register.token)

echo "catalog"
r=$(gql "$ACME" "$CREATE" '{"i":{"sku":"MUG","name":"Enamel mug","description":"camp mug","price":{"amount":"12.50","currency":"USD"},"initialStock":5}}')
check "admin creates product" "$r" '.data.createProduct.id'
MUG=$(echo "$r" | jq -r .data.createProduct.id)
r=$(gql "$ALICE" "$CREATE" '{"i":{"sku":"X","name":"X","price":{"amount":"1.00","currency":"USD"}}}')
check "customer cannot create product" "$r" '.errors[0].extensions.code == "FORBIDDEN"'
r=$(gql "$ALICE" 'query { products(query: "mug") { items { name available } } }')
check "full-text search" "$r" '.data.products.items[0].name == "Enamel mug"'

echo "orders"
r=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"o1-$RUN\",\"paymentMethod\":\"pm_ok\",\"lines\":[{\"productId\":\"$MUG\",\"qty\":2}]}}")
check "order confirmed" "$r" '.data.placeOrder.status == "CONFIRMED" and .data.placeOrder.total.amount == "25.00"'
ORDER=$(echo "$r" | jq -r .data.placeOrder.id)
r2=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"o1-$RUN\",\"paymentMethod\":\"pm_ok\",\"lines\":[{\"productId\":\"$MUG\",\"qty\":2}]}}")
check "retry returns the same order" "$r2" ".data.placeOrder.id == \"$ORDER\""
r=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"o2-$RUN\",\"paymentMethod\":\"pm_decline\",\"lines\":[{\"productId\":\"$MUG\",\"qty\":1}]}}")
check "declined card -> rejected, stock released" "$r" '.data.placeOrder.status == "REJECTED" and (.data.placeOrder.history | index("RELEASING_STOCK"))'
r=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"o3-$RUN\",\"paymentMethod\":\"pm_expire_auth\",\"lines\":[{\"productId\":\"$MUG\",\"qty\":1}]}}")
check "failed capture -> voided, rejected" "$r" '.data.placeOrder.status == "REJECTED" and (.data.placeOrder.history | index("VOIDING_PAYMENT"))'
r=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"o4-$RUN\",\"paymentMethod\":\"pm_ok\",\"lines\":[{\"productId\":\"$MUG\",\"qty\":99}]}}")
check "out of stock -> rejected" "$r" '.data.placeOrder.reason | startswith("out_of_stock")'
r=$(gql "$ALICE" "query { product(id: \"$MUG\") { available } }")
check "stock is 5 - 2 = 3" "$r" '.data.product.available == 3'
r=$(gql "$ACME" "query { order(id: \"$ORDER\") { payment { status fee { amount } } } }")
check "admin sees captured payment" "$r" '.data.order.payment.status == "captured"'

echo "isolation"
r=$(gql "$GLOBEX" "query { order(id: \"$ORDER\") { id } products { items { id } } }")
check "other shop sees nothing" "$r" '.data.order == null and (.data.products.items | length) == 0'
r=$(gql "not.a.token" 'query { me { id } }')
check "bad token rejected" "$r" '.errors[0].extensions.code == "UNAUTHENTICATED"'

echo "events"
events=$($COMPOSE exec -T redpanda rpk topic consume order.events -n 3 -f '%v\n' --offset start 2>/dev/null || true)
if [ "$(echo "$events" | grep -c '"status"')" -ge 3 ]; then ok "order events reached Kafka (outbox relay)"
else bad "order events reached Kafka (outbox relay)" "$events"; fi

LAB=${LAB:-http://localhost:${LAB_PORT:-8090}}
echo "choreographed saga (via Kafka)"
if curl -sf -X PUT "$LAB/config" -d '{"chaos":{},"paused":{},"patterns":{"saga":"choreography"}}' >/dev/null; then
  sleep 2 # services poll the switch every second
  r=$(gql "$ACME" "$CREATE" '{"i":{"sku":"TENT","name":"Tent","price":{"amount":"80.00","currency":"USD"},"initialStock":3}}')
  TENT=$(echo "$r" | jq -r .data.createProduct.id)
  r=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"c1-$RUN\",\"paymentMethod\":\"pm_ok\",\"lines\":[{\"productId\":\"$TENT\",\"qty\":1}]}}")
  C1=$(echo "$r" | jq -r .data.placeOrder.id)
  r=$(gql "$ALICE" "$PLACE" "{\"i\":{\"requestId\":\"c2-$RUN\",\"paymentMethod\":\"pm_decline\",\"lines\":[{\"productId\":\"$TENT\",\"qty\":1}]}}")
  C2=$(echo "$r" | jq -r .data.placeOrder.id)
  for _ in $(seq 30); do # events take a few hops; poll until both finish
    r=$(gql "$ALICE" "query { a: order(id: \"$C1\") { status } b: order(id: \"$C2\") { status reason } t: product(id: \"$TENT\") { available } }")
    echo "$r" | jq -e '.data.a.status == "CONFIRMED" and .data.b.status == "REJECTED"' >/dev/null 2>&1 && break
    sleep 1
  done
  check "choreography: paid order confirmed" "$r" '.data.a.status == "CONFIRMED"'
  check "choreography: declined order rejected, stock released" "$r" '.data.b.status == "REJECTED" and .data.t.available == 2'
  curl -sf -X PUT "$LAB/config" -d '{"chaos":{},"paused":{},"patterns":{"saga":"orchestration"}}' >/dev/null
else
  bad "choreographed saga" "lab not reachable at $LAB"
fi

echo "pattern lab"
if inv=$(curl -sf "$LAB/invariants"); then
  check "lab invariants hold (no oversell, ledger balanced, money matches orders)" "$inv" '[.[] | select(.kind == "invariant")] | length > 0 and all(.ok)'
else
  bad "lab invariants hold" "lab not reachable at $LAB"
fi

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
