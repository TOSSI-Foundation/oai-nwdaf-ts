#!/bin/bash
# Bring up the NWDAF containers needed for the closed loop, on a subnet that does
# NOT collide with the traffic-steering deployment.
#
# WHY THIS EXISTS (configuration change, permanent for the steering scenario):
# The upstream OAI NWDAF compose files and helper scripts both put
# oai-nwdaf-net on 192.168.74.0/24 - which is exactly the subnet
# docker-compose-basic-vpp-pcf-steering.yaml uses for oai-public-core-sec, the
# SECONDARY N6 network that Route 2 steers onto. Creating the NWDAF network as
# shipped would either fail or break the steering path. This script uses
# 192.168.75.0/24 instead; nothing else changes.
#
# WHAT IS STARTED
# The custom closed loop needs four containers (database, traffic-steering
# engine, nbi-events, sbi). The STANDARD NF_LOAD path the SMF consumes needs two
# more - oai-nwdaf-engine and oai-nwdaf-nbi-analytics - so all six are started
# here. oai-nwdaf-nbi-ml, -engine-ads and the Kong gateway are still not started.
#
# NRF ACCESS IS NOW REQUIRED for the standard path:
#   * oai-nwdaf-nbi-analytics REGISTERS the NWDAF (TS 23.288 clause 5.1/5.2), and
#   * oai-nwdaf-engine DISCOVERS the target UPF's NF Instance ID
#     (TS 23.288 clause 6.2.2.4) instead of using a pinned constant that goes
#     stale on every UPF restart.
# Both are therefore attached to the control-plane network as well.
#
#   sudo ./scripts/deploy/nwdaf_stack_up.sh
set -euo pipefail

NET=oai-nwdaf-net
PFX=${NWDAF_NET_PREFIX:-192.168.75}
CPNET=demo-oai-public-net
CPPFX=${CP_NET_PREFIX:-192.168.70}
MONGO="mongodb://${PFX}.156:27017"
DB=testing
NRF_URI=${NRF_URI:-http://${CPPFX}.130:8080}
# The NWDAF's own NF Instance ID, registered with the NRF. Stable by design -
# unlike the OAI NFs, which regenerate theirs on every start.
NWDAF_NF_INSTANCE_ID=${NWDAF_NF_INSTANCE_ID:-a1b2c3d4-5e6f-4a7b-8c9d-0e1f2a3b4c5d}
# Image tags. Override to roll back to the pre-hardening images.
TAG=${NWDAF_IMAGE_TAG:-nwdaf-hardening}
ENGINE_IMAGE=${ENGINE_IMAGE:-oai-nwdaf-engine:$TAG}
ANALYTICS_IMAGE=${ANALYTICS_IMAGE:-oai-nwdaf-nbi-analytics:$TAG}
EVENTS_IMAGE=${EVENTS_IMAGE:-oai-nwdaf-nbi-events:$TAG}
SBI_IMAGE=${SBI_IMAGE:-oai-nwdaf-sbi:$TAG}
STEERING_IMAGE=${STEERING_IMAGE:-oai-nwdaf-engine-traffic-steering:latest}
# DN_PERFORMANCE default analytics window, in seconds. This is the single
# largest term in the closed-loop reaction latency, so it
# is exposed here for measurement. 300 is the shipping default and the value
# that was hard-coded before it became configurable - leave it alone unless you
# are deliberately trading statistical support for speed: a shorter window has
# fewer samples in it, which lowers prediction Confidence.
DN_PERF_WINDOW_SEC=${ENGINE_DN_PERFORMANCE_WINDOW_SEC:-300}

if ! docker network inspect "$NET" >/dev/null 2>&1; then
  echo "==> creating $NET (${PFX}.0/24)"
  docker network create --driver bridge --subnet "${PFX}.0/24" \
    -o com.docker.network.bridge.name=cn5g-nwdaf "$NET"
else
  echo "==> $NET already exists: $(docker network inspect "$NET" \
        --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')"
fi

# --type container matters: a bare `docker inspect NAME` also matches the IMAGE
# of the same name, so every service looked "already created".
up(){ docker inspect --type container "$1" >/dev/null 2>&1 \
        && { echo "==> $1 container already exists, skipping"; return 1; }; return 0; }

if up oai-nwdaf-database; then
echo "==> oai-nwdaf-database (${PFX}.156, host port 27017)"
docker run -d --name oai-nwdaf-database --network "$NET" --ip "${PFX}.156" \
  -p 27017:27017 --restart always mongo:latest >/dev/null
fi

# OPTIONAL - the ML congestion-forecast engine (Track 1). Not on the RATE or
# HEALTH steering path; start_nwdaf.sh sets SKIP_STEERING_ENGINE when its image
# is absent, which is the normal case for a fresh clone of this repository.
if [ -z "${SKIP_STEERING_ENGINE:-}" ] && up oai-nwdaf-engine-traffic-steering; then
echo "==> oai-nwdaf-engine-traffic-steering (${PFX}.159)"
docker run -d --name oai-nwdaf-engine-traffic-steering --network "$NET" --ip "${PFX}.159" \
  -e SERVER_PORT=8080 \
  -e MONGODB_URI="$MONGO" -e MONGODB_DATABASE_NAME="$DB" \
  -e MONGODB_COLLECTION_NAME_SMF=smf \
  -e MONGODB_COLLECTION_NAME_UPF_METRICS=upf_metrics \
  -e MONGODB_COLLECTION_NAME_TRAFFIC_STEERING_PREDICTIONS=traffic_steering_predictions \
  -e TRAFFIC_STEERING_UPF_ID=vpp-upf \
  -e TRAFFIC_STEERING_HORIZON_SEC=30 \
  -e TRAFFIC_STEERING_FEATURE_WINDOW_SEC=60 \
  "$STEERING_IMAGE" >/dev/null
fi

if up oai-nwdaf-nbi-events; then
echo "==> oai-nwdaf-nbi-events (${PFX}.152, host port 6060)"
docker run -d --name oai-nwdaf-nbi-events --network "$NET" --ip "${PFX}.152" \
  -p 6060:8080 \
  -e SERVER_ADDR=0.0.0.0:8080 \
  `# EVENTS_URI is the apiRoot published in the 201 Location header` \
  `# (TS 29.520 POST /subscriptions). Unset, it defaults to localhost:8882,` \
  `# which no consumer can dereference.` \
  -e EVENTS_URI="http://${PFX}.152:8080" \
  -e ENGINE_URI="http://${PFX}.155:8080" \
  -e ENGINE_ADS_URI="http://${PFX}.157:8080" \
  -e ENGINE_TRAFFIC_STEERING_URI="http://${PFX}.159:8080" \
  -e ENGINE_NUM_OF_UE_ROUTE=/network_performance/num_of_ue \
  -e ENGINE_SESS_SUCC_RATIO_ROUTE=/network_performance/sess_succ_ratio \
  -e ENGINE_UE_COMMUNICATION_ROUTE=/ue_communication \
  -e ENGINE_UE_MOBILITY_ROUTE=/ue_mobility \
  -e ENGINE_NF_LOAD_ROUTE=/nf_load \
  -e ENGINE_QOS_SUSTAINABILITY_ROUTE=/qos_sustainability \
  -e ENGINE_DN_PERFORMANCE_ROUTE=/dn_performance \
  -e ENGINE_UNEXPECTED_LARGE_RATE_FLOW_ROUTE=/abnormal_behaviour/unexpected_large_rate_flow \
  -e ENGINE_TRAFFIC_STEERING_ROUTE=/traffic_steering/congestion_forecast \
  "$EVENTS_IMAGE" >/dev/null
fi

if up oai-nwdaf-engine; then
echo "==> oai-nwdaf-engine (${PFX}.155 + ${CPPFX}.196, host port 6063)"
docker run -d --name oai-nwdaf-engine --network "$NET" --ip "${PFX}.155" \
  -p 6063:8080 \
  -e SERVER_ADDR=0.0.0.0:8080 \
  -e MONGODB_URI="$MONGO" -e MONGODB_DATABASE_NAME="$DB" \
  -e MONGODB_COLLECTION_NAME_AMF=amf -e MONGODB_COLLECTION_NAME_SMF=smf \
  -e MONGODB_COLLECTION_NAME_UPF_METRICS=upf_metrics \
  -e ENGINE_NUM_OF_UE_ROUTE=/network_performance/num_of_ue \
  -e ENGINE_SESS_SUCC_RATIO_ROUTE=/network_performance/sess_succ_ratio \
  -e ENGINE_UE_COMMUNICATION_ROUTE=/ue_communication \
  -e ENGINE_UE_MOBILITY_ROUTE=/ue_mobility \
  -e ENGINE_NF_LOAD_ROUTE=/nf_load \
  -e ENGINE_QOS_SUSTAINABILITY_ROUTE=/qos_sustainability \
  -e ENGINE_DN_PERFORMANCE_ROUTE=/dn_performance \
  -e ENGINE_DN_PERFORMANCE_WINDOW_SEC="$DN_PERF_WINDOW_SEC" \
  -e NF_LOAD_UPF_ID=vpp-upf \
  -e NF_LOAD_UPF_CPU_CORES=16 \
  -e NF_LOAD_UPF_MEM_CAPACITY_BYTES=24000000000 \
  `# NF_LOAD_UPF_NF_INSTANCE_ID is deliberately UNSET - the NF Instance ID is` \
  `# resolved from the NRF. Setting it pins a value that goes stale on the` \
  `# next UPF restart.` \
  -e NRF_URI="$NRF_URI" -e NRF_HTTP_VERSION=2 \
  -e NF_LOAD_UPF_NRF_FQDN=vpp-upf \
  -e NF_LOAD_UPF_NRF_REFRESH_SEC=30 \
  "$ENGINE_IMAGE" >/dev/null
docker network connect --ip "${CPPFX}.196" "$CPNET" oai-nwdaf-engine
fi

if up oai-nwdaf-nbi-analytics; then
echo "==> oai-nwdaf-nbi-analytics (${PFX}.151 + ${CPPFX}.198, host port 6059)"
docker run -d --name oai-nwdaf-nbi-analytics --network "$NET" --ip "${PFX}.151" \
  -p 6059:8080 \
  -e SERVER_ADDR=0.0.0.0:8080 \
  -e ENGINE_URI="http://${PFX}.155:8080" \
  -e ENGINE_TRAFFIC_STEERING_URI="http://${PFX}.159:8080" \
  -e ENGINE_NUM_OF_UE_ROUTE=/network_performance/num_of_ue \
  -e ENGINE_SESS_SUCC_RATIO_ROUTE=/network_performance/sess_succ_ratio \
  -e ENGINE_UE_COMMUNICATION_ROUTE=/ue_communication \
  -e ENGINE_UE_MOBILITY_ROUTE=/ue_mobility \
  -e ENGINE_NF_LOAD_ROUTE=/nf_load \
  -e ENGINE_QOS_SUSTAINABILITY_ROUTE=/qos_sustainability \
  -e ENGINE_DN_PERFORMANCE_ROUTE=/dn_performance \
  -e ENGINE_TRAFFIC_STEERING_ROUTE=/traffic_steering/congestion_forecast \
  -e NRF_URI="$NRF_URI" -e NRF_HTTP_VERSION=2 \
  -e NWDAF_NF_INSTANCE_ID="$NWDAF_NF_INSTANCE_ID" \
  -e NWDAF_ANALYTICS_IPV4="${CPPFX}.198" -e NWDAF_ANALYTICS_PORT=8080 \
  -e NWDAF_EVENTS_IPV4="${PFX}.152" -e NWDAF_EVENTS_PORT=8080 \
  -e NWDAF_HEARTBEAT_SEC=10 \
  "$ANALYTICS_IMAGE" >/dev/null
docker network connect --ip "${CPPFX}.198" "$CPNET" oai-nwdaf-nbi-analytics
fi

# ---------------------------------------------------------------------------
# LAST, and only ONCE, and only after AMF + SMF are healthy.
#
# oai-nwdaf-sbi retries with backoff,
# persists what it created, deletes it before subscribing again, and unsubscribes
# on SIGTERM. BUT neither the OAI SMF nor the OAI AMF implements the Individual
# Subscription resource, so that DELETE cannot succeed (§17.5). An extra start
# against a STILL-RUNNING SMF therefore still leaves a stale subscription behind;
# only restarting the SMF clears them, because it holds them in memory.
#
# Historically: every extra start added a DUPLICATE SMF subscription and every usage report was
# then stored N times, inflating all summed rates N-fold.
# ---------------------------------------------------------------------------
if up oai-nwdaf-sbi; then
echo "==> oai-nwdaf-sbi (${PFX}.158 + 192.168.70.158, host port 6062)"
docker run -d --name oai-nwdaf-sbi --network "$NET" --ip "${PFX}.158" \
  -p 6062:8080 \
  -e SERVER_ADDR=0.0.0.0:8080 \
  -e MONGODB_URI="$MONGO" -e MONGODB_DATABASE_NAME="$DB" \
  -e MONGODB_COLLECTION_NAME_AMF=amf -e MONGODB_COLLECTION_NAME_SMF=smf \
  -e EVENT_NOTIFY_URI=http://oai-nwdaf-sbi:8080 \
  -e AMF_IP_ADDR=http://192.168.70.132:8080 -e AMF_HTTP_VERSION=2 \
  -e AMF_API_ROUTE=/test/amf -e AMF_SUBSCR_ROUTE=/namf-evts/v1 \
  -e AMF_NOTIFICATION_ID=1 -e AMF_NOTIFY_CORRELATION_ID=string \
  -e AMF_NOTIFICATION_FORWARD_ROUTE=/sbi/notification/amf \
  -e SMF_IP_ADDR=http://192.168.70.133:8080 -e SMF_HTTP_VERSION=2 \
  -e SMF_API_ROUTE=/test/smf \
  `# The sbi image ships SMF_SUBSCR_ROUTE=/nsmf_event-exposure/v1, which is the` \
  `# 3GPP TS 29.508 service name. This SMF registers its handler under` \
  `# /nsmf-event-exposure/ (sbi_helper.hpp SmfEventExposureBase, hyphen, not` \
  `# underscore), so the spec-correct path 404s. Overridden here rather than` \
  `# patched in the SMF: the sbi route is configuration, the SMF path is baked` \
  `# into the URIs it advertises to the NRF.` \
  -e SMF_SUBSCR_ROUTE=/nsmf-event-exposure/v1 \
  -e SMF_NOTIFICATION_ID=2 -e SMF_NOTIFY_CORRELATION_ID=string \
  -e SMF_NOTIFICATION_FORWARD_ROUTE=/sbi/notification/smf \
  `# OPTIONAL: lets the collector notice that an AMF/SMF restarted, by watching` \
  `# its NF Instance ID in the NRF (TS 23.288 clause 6.2.2.4). Without it a peer` \
  `# restart silently ends data collection until this container is restarted.` \
  -e NRF_URI="$NRF_URI" -e NRF_HTTP_VERSION=2 \
  "$SBI_IMAGE" >/dev/null
# docker run takes only one --network; sbi also needs the control-plane net
docker network connect --ip 192.168.70.158 "$CPNET" oai-nwdaf-sbi
fi

echo "==> up:"
docker ps --format '{{.Names}}\t{{.Status}}' | grep nwdaf
