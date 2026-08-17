#!/bin/sh
# Create or reconcile the API role used by both Qumulo COSI and CSI
# controllers. Ordinary filesystem access is granted separately on the
# configured base paths; this role deliberately omits FILE_FULL_ACCESS.
# FS_DELETE_TREE_WRITE is the important exception: Qumulo documents that it
# can delete any file or directory cluster-wide regardless of filesystem ACLs.
# It is required by the packaged CSI deleteData=true default and COSI purge.
#
# Usage: ./hack/create-driver-role.sh [user] [role]
# Then, as an admin: qq auth_create_access_token local:<user>
set -eu

USER_NAME="${1:-cosi-driver}"
ROLE_NAME="${2:-cosi-driver-role}"

# These are the current Core privilege names. Validate the complete set before
# changing the user or role so an older/incompatible cluster fails closed.
PRIVS="
PRIVILEGE_S3_BUCKETS_READ
PRIVILEGE_S3_BUCKETS_WRITE
PRIVILEGE_S3_SETTINGS_READ
PRIVILEGE_S3_CREDENTIALS_READ
PRIVILEGE_S3_CREDENTIALS_WRITE
PRIVILEGE_LOCAL_GROUP_READ
PRIVILEGE_LOCAL_USER_READ
PRIVILEGE_LOCAL_USER_WRITE
PRIVILEGE_FS_ATTRIBUTES_READ
PRIVILEGE_FS_DELETE_TREE_READ
PRIVILEGE_FS_DELETE_TREE_WRITE
PRIVILEGE_S3_UPLOADS_READ
PRIVILEGE_S3_UPLOADS_WRITE
PRIVILEGE_QUOTA_READ
PRIVILEGE_QUOTA_WRITE
PRIVILEGE_NFS_EXPORT_READ
PRIVILEGE_NFS_EXPORT_WRITE
PRIVILEGE_SMB_SHARE_READ
PRIVILEGE_SMB_SHARE_WRITE
"

available=$(qq auth_list_privileges)
missing=""
# shellcheck disable=SC2086
for priv in $PRIVS; do
  case "$available" in
    *"$priv"*) ;;
    *) missing="$missing $priv" ;;
  esac
done
if [ -n "$missing" ]; then
  echo "Core does not advertise required privileges:$missing" >&2
  echo "No user or role changes were made." >&2
  exit 1
fi

if qq auth_list_user --id "$USER_NAME" >/dev/null 2>&1; then
  echo "Using existing local user $USER_NAME"
else
  qq auth_add_user --name "$USER_NAME"
fi

# auth_find_identity returns one canonical identity object. Do not use
# auth_expand_identity here: its equivalent/group identities can carry other
# auth_ids and JSON key ordering is not an identity boundary.
identity_json=$(qq auth_find_identity "local:$USER_NAME" --json)
USER_AUTH_ID=$(printf '%s\n' "$identity_json" | tr ',' '\n' | sed -n 's/.*"auth_id"[[:space:]]*:[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}[[:space:]}]*$/\1/p' | sed -n '1p')
if [ -z "$USER_AUTH_ID" ]; then
  echo "Could not resolve the immutable auth_id for local:$USER_NAME" >&2
  exit 1
fi

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT HUP INT TERM
# shellcheck disable=SC2086
printf '%s\n' $PRIVS > "$tmp"
if qq auth_list_role --role "$ROLE_NAME" >/dev/null 2>&1; then
  # Never upgrade a shared role in place. Otherwise every pre-existing member
  # would silently receive S3 credential administration and cluster-wide tree
  # deletion. Auth IDs are immutable and unique within the cluster.
  existing_role_json=$(qq auth_list_role --role "$ROLE_NAME" --json)
  member_auth_ids=$(printf '%s\n' "$existing_role_json" | tr ',' '\n' | sed -n 's/.*"auth_id"[[:space:]]*:[[:space:]]*"\{0,1\}\([0-9][0-9]*\)"\{0,1\}[[:space:]}]*$/\1/p')
  for member_auth_id in $member_auth_ids; do
    if [ "$member_auth_id" != "$USER_AUTH_ID" ]; then
      echo "Refusing to modify shared role $ROLE_NAME: it has an unexpected member auth_id $member_auth_id" >&2
      exit 1
    fi
  done
  qq auth_modify_role \
    --role "$ROLE_NAME" \
    --description "Qumulo COSI and CSI storage controllers" \
    --privileges-file "$tmp"
else
  qq auth_create_role \
    --role "$ROLE_NAME" \
    --description "Qumulo COSI and CSI storage controllers" \
    --privileges-file "$tmp"
fi

granted=$(qq auth_list_privileges --role "$ROLE_NAME")
# shellcheck disable=SC2086
for priv in $PRIVS; do
  case "$granted" in
    *"$priv"*) ;;
    *) echo "Role verification failed: $priv was not granted" >&2; exit 1 ;;
  esac
done

# Always qualify the LOCAL identity. Bare names are ambiguous when a cluster
# also has AD or LDAP identities with the same display name.
qq auth_assign_role --role "$ROLE_NAME" --trustee "local:$USER_NAME" >/dev/null 2>&1 || true
role_json=$(qq auth_list_role --role "$ROLE_NAME" --json)
if ! printf '%s' "$role_json" | tr -d '[:space:]' | grep -Eq "\"auth_id\":\"?$USER_AUTH_ID\"?([,}])"; then
  echo "Could not verify local:$USER_NAME (auth_id $USER_AUTH_ID) in $ROLE_NAME" >&2
  exit 1
fi

echo
echo "Mint a token (requires an admin session):"
echo "  qq auth_create_access_token local:$USER_NAME"
echo "Store it in Kubernetes:"
echo "  kubectl create secret generic qumulo-cosi-creds -n qumulo-cosi --from-literal=token='...'"
echo
echo "Grant path-scoped filesystem access after creating the configured base paths:"
echo "  qq fs_modify_acl --path /k8s-buckets add_entry --trustee local:$USER_NAME --type Allowed --rights All --flags 'Container inherit' 'Object inherit'"
echo "  qq fs_modify_acl --path /k8s-volumes add_entry --trustee local:$USER_NAME --type Allowed --rights All --flags 'Container inherit' 'Object inherit'"
echo
echo "WARNING: PRIVILEGE_FS_DELETE_TREE_WRITE bypasses filesystem ACLs and can"
echo "delete data anywhere on the cluster. Protect this token as a destructive"
echo "cluster-wide credential; see docs/qumulo-setup.md before production use."
