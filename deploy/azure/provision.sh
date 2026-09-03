#!/usr/bin/env bash
# Provision the Azure infrastructure for the Aikonos dev deployment:
# VNet + subnet, NSG (443/80 public, 22 admin-only), static Public IP with a DNS
# label, and an Ubuntu VM (Docker via cloud-init) with a Premium data disk for
# durable Docker volumes. All resources land in the existing resource group.
#
# Prereqs: `az login` with Contributor on the subscription; an SSH public key.
# Idempotency: az resource creates are NOT fully idempotent — re-running may error
# on already-existing resources. Inspect/delete partials before re-running.
set -euo pipefail

# ── Config (override via env before running) ───────────────────────────────────
SUBSCRIPTION_ID="${SUBSCRIPTION_ID:-61d89240-3dd5-4707-9eea-59dfa38d72ea}"
RG="${RG:-rg-tiakon-dev-weu-001}"
LOCATION="${LOCATION:-westeurope}"

VNET="${VNET:-vnet-aikonos-dev-weu-001}"
SUBNET="${SUBNET:-snet-app}"
NSG="${NSG:-nsg-aikonos-dev}"
PIP="${PIP:-pip-aikonos-dev}"
NIC="${NIC:-nic-aikonos-dev}"
VM="${VM:-vm-aikonos-dev}"

# DNS label → <label>.westeurope.cloudapp.azure.com (must be unique within region).
DNS_LABEL="${DNS_LABEL:-aikonos-dev}"
VM_SIZE="${VM_SIZE:-Standard_D8s_v5}"        # 8 vCPU / 32 GB; min Standard_D4s_v5 (16 GB)
VM_IMAGE="${VM_IMAGE:-Ubuntu2404}"
ADMIN_USER="${ADMIN_USER:-azureuser}"
ADMIN_SSH_KEY="${ADMIN_SSH_KEY:-$HOME/.ssh/id_rsa.pub}"
OS_DISK_GB="${OS_DISK_GB:-64}"
DATA_DISK_GB="${DATA_DISK_GB:-128}"

# Restrict SSH to the IP you run this from. Override ADMIN_IP to set another.
ADMIN_IP="${ADMIN_IP:-$(curl -fsS https://api.ipify.org)}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Subscription + resource group"
az account set --subscription "$SUBSCRIPTION_ID"
az group show -n "$RG" >/dev/null   # fail early if the RG is missing

echo "==> VNet + subnet ($VNET / $SUBNET)"
az network vnet create -g "$RG" -n "$VNET" -l "$LOCATION" \
  --address-prefixes 10.0.0.0/16 \
  --subnet-name "$SUBNET" --subnet-prefixes 10.0.1.0/24 -o none

echo "==> NSG + rules ($NSG): 443/80 public, 22 from ${ADMIN_IP}/32"
az network nsg create -g "$RG" -n "$NSG" -l "$LOCATION" -o none
az network nsg rule create -g "$RG" --nsg-name "$NSG" -n allow-https \
  --priority 100 --access Allow --protocol Tcp --direction Inbound \
  --destination-port-ranges 443 --source-address-prefixes '*' -o none
az network nsg rule create -g "$RG" --nsg-name "$NSG" -n allow-http \
  --priority 110 --access Allow --protocol Tcp --direction Inbound \
  --destination-port-ranges 80 --source-address-prefixes '*' -o none
az network nsg rule create -g "$RG" --nsg-name "$NSG" -n allow-ssh-admin \
  --priority 120 --access Allow --protocol Tcp --direction Inbound \
  --destination-port-ranges 22 --source-address-prefixes "${ADMIN_IP}/32" -o none

echo "==> Attach NSG to subnet"
az network vnet subnet update -g "$RG" --vnet-name "$VNET" -n "$SUBNET" \
  --network-security-group "$NSG" -o none

echo "==> Public IP ($PIP, static, DNS label $DNS_LABEL)"
az network public-ip create -g "$RG" -n "$PIP" -l "$LOCATION" \
  --sku Standard --allocation-method Static --dns-name "$DNS_LABEL" -o none

echo "==> NIC ($NIC)"
az network nic create -g "$RG" -n "$NIC" -l "$LOCATION" \
  --vnet-name "$VNET" --subnet "$SUBNET" \
  --public-ip-address "$PIP" --network-security-group "$NSG" -o none

echo "==> VM ($VM, $VM_SIZE, $VM_IMAGE) + ${DATA_DISK_GB}GB Premium data disk + cloud-init"
az vm create -g "$RG" -n "$VM" -l "$LOCATION" \
  --image "$VM_IMAGE" --size "$VM_SIZE" \
  --admin-username "$ADMIN_USER" --ssh-key-values "$ADMIN_SSH_KEY" \
  --nics "$NIC" \
  --os-disk-size-gb "$OS_DISK_GB" --storage-sku Premium_LRS \
  --data-disk-sizes-gb "$DATA_DISK_GB" \
  --custom-data "${SCRIPT_DIR}/cloud-init.yaml" -o none

FQDN="$(az network public-ip show -g "$RG" -n "$PIP" --query dnsSettings.fqdn -o tsv)"
echo
echo "==> Done."
echo "    FQDN:  $FQDN"
echo "    SSH:   ssh ${ADMIN_USER}@${FQDN}"
echo
echo "Next:"
echo "  1) ./entra-setup.sh $FQDN          # create the two Entra app registrations"
echo "  2) ssh in, clone the repo, prepare .env from deploy/compose/.env.azure.example"
echo "  3) follow deploy/azure/README.md bring-up"
