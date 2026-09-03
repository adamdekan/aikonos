#!/usr/bin/env bash
# Create the two single-tenant Entra ID app registrations Aikonos needs:
#   App A  aikonos-webui        — SPA (PKCE) + exposes api://<id>/access_as_user.
#                                Its access token is what the broker/gateway validate.
#   App B  aikonos-oauth2-proxy — confidential Web app for the edge gate (oauth2-proxy).
#
# Single-tenant ("AzureADMyOrg") on both = only this directory's users can sign in.
# Mirrors docs/12-entra-login.md §"Entra portal setup", done via Microsoft Graph.
#
# Prereqs: `az login` as a user allowed to create app registrations (and ideally
# grant admin consent). Uses `az rest` for the SPA-platform + expose-API parts the
# `az ad app` verbs don't cover cleanly.
set -euo pipefail

FQDN="${1:?usage: entra-setup.sh <public-fqdn>   e.g. aikonos-dev.westeurope.cloudapp.azure.com}"
TENANT_ID="$(az account show --query tenantId -o tsv)"

uuid() { cat /proc/sys/kernel/random/uuid; }

echo "==> Tenant: $TENANT_ID"
echo "==> FQDN:   $FQDN"

# ── App A: aikonos-webui (SPA + exposed API) ────────────────────────────────────
echo "==> Creating App A: aikonos-webui"
WEBUI_APP_ID="$(az ad app create --display-name aikonos-webui \
  --sign-in-audience AzureADMyOrg --query appId -o tsv)"
WEBUI_OBJ_ID="$(az ad app show --id "$WEBUI_APP_ID" --query id -o tsv)"
az ad app update --id "$WEBUI_APP_ID" --identifier-uris "api://$WEBUI_APP_ID" -o none

SCOPE_ID="$(uuid)"
echo "==> App A: SPA redirect + expose api://.../access_as_user (+ pre-authorize self)"
az rest --method PATCH \
  --uri "https://graph.microsoft.com/v1.0/applications/$WEBUI_OBJ_ID" \
  --headers "Content-Type=application/json" \
  --body @- >/dev/null <<JSON
{
  "spa": { "redirectUris": ["https://$FQDN/auth/callback"] },
  "api": {
    "oauth2PermissionScopes": [{
      "id": "$SCOPE_ID",
      "value": "access_as_user",
      "type": "User",
      "isEnabled": true,
      "adminConsentDisplayName": "Access Aikonos",
      "adminConsentDescription": "Access Aikonos as the signed-in user",
      "userConsentDisplayName": "Access Aikonos",
      "userConsentDescription": "Access Aikonos as you"
    }],
    "preAuthorizedApplications": [{
      "appId": "$WEBUI_APP_ID",
      "delegatedPermissionIds": ["$SCOPE_ID"]
    }]
  }
}
JSON

# Enterprise app (service principal) so users can sign in.
az ad sp create --id "$WEBUI_APP_ID" -o none 2>/dev/null || true

# ── App B: aikonos-oauth2-proxy (confidential Web app for the edge gate) ─────────
echo "==> Creating App B: aikonos-oauth2-proxy"
PROXY_APP_ID="$(az ad app create --display-name aikonos-oauth2-proxy \
  --sign-in-audience AzureADMyOrg \
  --web-redirect-uris "https://$FQDN/oauth2/callback" \
  --query appId -o tsv)"
az ad sp create --id "$PROXY_APP_ID" -o none 2>/dev/null || true
PROXY_SECRET="$(az ad app credential reset --id "$PROXY_APP_ID" \
  --display-name oauth2-proxy --query password -o tsv)"

COOKIE_SECRET="$(openssl rand -base64 32)"

cat <<OUT

================================================================================
Entra app registrations created. Paste these into the VM's .env
(from deploy/compose/.env.azure.example):

  ENTRA_TENANT_ID=$TENANT_ID
  AIKONOS_TENANT_ID=$TENANT_ID

  # App A (webui SPA + API audience)
  AIKONOS_OIDC_AUDIENCE=$WEBUI_APP_ID
  AIKONOS_WEBUI_OIDC_CLIENT=$WEBUI_APP_ID
  AIKONOS_WEBUI_OIDC_SCOPE=openid profile api://$WEBUI_APP_ID/access_as_user
  AIKONOS_OIDC_ISSUER=https://login.microsoftonline.com/$TENANT_ID/v2.0
  AIKONOS_OIDC_JWKS_URL=https://login.microsoftonline.com/$TENANT_ID/discovery/v2.0/keys
  AIKONOS_WEBUI_OIDC_AUTHORITY=https://login.microsoftonline.com/$TENANT_ID/v2.0
  AIKONOS_WEBUI_OIDC_REDIRECT_URI=https://$FQDN/auth/callback

  # App B (oauth2-proxy edge gate)
  OAUTH2_PROXY_CLIENT_ID=$PROXY_APP_ID
  OAUTH2_PROXY_CLIENT_SECRET=$PROXY_SECRET
  OAUTH2_PROXY_COOKIE_SECRET=$COOKIE_SECRET

  AIKONOS_FQDN=$FQDN

NOTE: if your tenant requires admin consent, an admin must grant it once for
App A's access_as_user scope (Portal → App registrations → aikonos-webui → API
permissions → Grant admin consent), or run:
  az ad app permission admin-consent --id $WEBUI_APP_ID
================================================================================
OUT
