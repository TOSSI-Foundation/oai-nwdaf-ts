# Thin wrapper over scripts/. Every target is one script you can also run
# directly - nothing is hidden here, and nothing only works via make.
#
#   make            list the targets
#   make up         core + NWDAF + UEs, end to end
#
.DEFAULT_GOAL := help
SHELL := /bin/bash

UES     ?= 5
ANCHORS ?= 1
RULE    ?=
MBPS    ?= 60
SECS    ?= 1800
PROTO   ?= udp
IFACE   ?= n6-3
RATE    ?= 40mbit

.PHONY: help build build-nwdaf build-nfs build-gnbsim build-fed \
        up core nwdaf ues load steer unsteer status verify clean logs \
        test test-rate test-health

help:
	@echo "First time on this machine:"
	@echo "  make build           build every image this deployment needs"
	@echo "  make build-nwdaf     ... only the NWDAF Go services   (~2 min)"
	@echo "  make build-nfs       ... only SMF, PCF, NRF           (HOURS)"
	@echo "  make build-gnbsim    ... only the UE simulator"
	@echo "  make build-fed       ... only the oai-cn5g-fed tree + configs"
	@echo
	@echo "Bring-up (in order):"
	@echo "  make core            5G core + the steering SMF (asks RATE or HEALTH)"
	@echo "  make core RULE=RATE  ... or choose non-interactively"
	@echo "  make nwdaf           NWDAF stack + host pollers"
	@echo "  make ues             attach UEs and the SBI      [UES=5 ANCHORS=1]"
	@echo "  make up              all three, end to end"
	@echo
	@echo "Testing:"
	@echo "  make test-health     automated HEALTH steering test  [WAIT=180 RATE=40mbit]"
	@echo "  make test-rate       automated RATE steering test    [WAIT=300]"
	@echo "  make load            traffic on every UE         [MBPS=60 SECS=1800 PROTO=udp]"
	@echo "  make steer           degrade a path to trigger a steer  [IFACE=n6-3 RATE=40mbit]"
	@echo "  make unsteer         remove the impairment       <- ALWAYS run this"
	@echo "  make status          where each session is, and path health"
	@echo "  make logs            follow the SMF steering decisions"
	@echo
	@echo "Other:"
	@echo "  make verify          offline build/test of this checkout (touches no lab)"
	@echo "  make clean           stop and remove everything (keeps MongoDB data)"
	@echo
	@echo "Full walkthrough, configuration reference and troubleshooting: README.md"

# Images first - every bring-up target below assumes the tags already exist.
# One target per thing build.sh can build, so the name says what it does and
# shell completion can find it. Each is still just './scripts/build.sh <arg>'.
build:
	@./scripts/build.sh all

build-nwdaf:
	@./scripts/build.sh nwdaf

build-nfs:
	@./scripts/build.sh nfs

build-gnbsim:
	@./scripts/build.sh gnbsim

build-fed:
	@./scripts/build.sh fed

up: core nwdaf ues
	@echo "Stack is up. Next: make load, then make steer"

core:
	@./scripts/deploy/start_core.sh $(if $(RULE),--rule $(RULE),)

nwdaf:
	@./scripts/deploy/start_nwdaf.sh

ues:
	@./scripts/lab/demo_reset_multi.sh $(UES) $(ANCHORS)

load:
	@./scripts/lab/demo_load_multi.sh $(MBPS) $(SECS) $(PROTO)

# Shapes one N6 interface so the path starts stalling. This is the ONLY way to
# produce degradation in this lab - nothing here can saturate a path on its own.
steer:
	@sudo docker exec vpp-upf tc qdisc add dev $(IFACE) root tbf rate $(RATE) burst 64kbit latency 400ms
	@echo "$(IFACE) shaped to $(RATE). Watch: make logs   Undo: make unsteer"

# A leftover qdisc silently breaks every later test, and the delete is silent
# whether or not it did anything - so this prints the resulting state.
unsteer:
	@for d in n6-3 n6-4; do sudo docker exec vpp-upf tc qdisc del dev $$d root 2>/dev/null || true; done
	@for d in n6-3 n6-4; do printf "  %-5s " $$d; sudo docker exec vpp-upf tc qdisc show dev $$d | head -1; done

status:
	@sudo docker exec vpp-upf /openair-upf/bin/vppctl show upf session 2>/dev/null \
	 | awk '/^CP F-SEID/{n=$$0;sub(/.*\(/,"",n);sub(/\).*/,"",n)} \
	        /IPv4 address: 12\./{if(!ip[n])ip[n]=$$3} \
	        /Network Instance: internet/{if(!w[n]){w[n]=$$3; print "  SEID "n"  "ip[n]"  "$$3}}'
	@sudo docker logs oai-smf --since 20s 2>&1 | grep -oP 'dnai=\S+ state=\S+' | sort -u | sed 's/^/  /' || true
	@echo -n "  rule: "; sudo docker logs oai-smf 2>&1 | awk '/ranking rule: HEALTH/{h=1} /NWDAF consumer starting/{last=h; h=0} END{print (last?"HEALTH":"RATE")}'


logs:
	@sudo docker logs -f oai-smf 2>&1 | grep --line-buffered -E \
	  "Steering cycle:|per-session decision|analytics preference|Steering: Update|dnai="

verify:
	@./scripts/verify_release.sh

# The two steering rules, each with a PASS/FAIL verdict verified against the
# UPF's forwarding state rather than against a log line. test-health impairs a
# path and ALWAYS removes the qdisc again, including on Ctrl-C.
#
# WAIT overrides how long each waits for a steer. test-rate is given longer
# because DN_PERFORMANCE averages over a 300 s window, so the rate separation has
# to build before there is anything to rank.
test-health:
	@./scripts/test-health.sh $(or $(WAIT),180) $(RATE)

test-rate:
	@./scripts/test-rate.sh $(or $(WAIT),300)

# NOT 'test: test-health test-rate'. The two rules are mutually exclusive - they
# are selected by SMF_NWDAF_DNPERF_RULE on the SMF container - so running them
# back to back always fails whichever one the SMF is not configured for. Each
# needs its own core bring-up, and RATE additionally needs a different traffic
# shape (anchor loaded, steerable UEs idle), so this cannot be one target.
test:
	@echo "The two rules cannot run back to back - the SMF is built for one at a time."
	@echo
	@echo "  HEALTH:  make core RULE=HEALTH && make ues && make load"
	@echo "           make test-health"
	@echo
	@echo "  RATE:    SMF_NWDAF_PREDICT_SEC=0 make core RULE=RATE && make ues"
	@echo "           load the ANCHOR only, leave the steerable UEs idle, then"
	@echo "           make test-rate"
	@echo
	@echo "See README.md section 8 (Testing)."
	@false

# Removes containers and networks but NOT volumes: the NWDAF MongoDB is on an
# anonymous volume, and 'docker-compose down -v' would destroy every metric ever
# collected. Standalone containers are removed FIRST - otherwise their network
# endpoints block compose from removing the networks.
clean:
	@for c in $$(sudo docker ps --format '{{.Names}}' --filter name=gnbsim-vpp); do \
	  sudo docker exec $$c pkill iperf3 2>/dev/null || true; done
	@sudo docker rm -f oai-nwdaf-sbi $$(sudo docker ps -aq --filter 'name=gnbsim-vpp') 2>/dev/null || true
	@sudo docker rm -f oai-nwdaf-engine oai-nwdaf-nbi-analytics oai-nwdaf-nbi-events \
	   oai-nwdaf-engine-traffic-steering oai-nwdaf-database oai-smf 2>/dev/null || true
	@cd $${FED:-$$HOME/oai-cn5g-fed/docker-compose} && \
	   { sudo docker compose version >/dev/null 2>&1 && C="docker compose" || C=docker-compose; } && \
	   sudo $$C -f docker-compose-basic-vpp-pcf-steering.yaml down 2>&1 | tail -3 || true
	@for n in demo-oai-public-net oai-public-access oai-public-core-pri oai-public-core-sec oai-nwdaf-net; do \
	   sudo docker network rm $$n >/dev/null 2>&1 || true; done
	@echo "  containers left: $$(sudo docker ps -q | wc -l)   (volumes kept)"
