{
  "realm": "booth",
  "enabled": true,
  "sslRequired": "none",
  "groups": [
    {
      "name": "workspaces",
      "subGroups": [
        {"name": "acme-analytics", "subGroups": [{"name": "owner"}, {"name": "editor"}, {"name": "viewer"}]},
        {"name": "other-team", "subGroups": [{"name": "owner"}, {"name": "editor"}, {"name": "viewer"}]}
      ]
    }
  ],
  "clients": [
    {
      "clientId": "booth-design",
      "enabled": true,
      "publicClient": true,
      "standardFlowEnabled": true,
      "directAccessGrantsEnabled": true,
      "redirectUris": ["*"],
      "webOrigins": ["*"],
      "attributes": {"pkce.code.challenge.method": "S256"},
      "protocolMappers": [
        {
          "name": "groups",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-group-membership-mapper",
          "config": {
            "claim.name": "groups",
            "full.path": "true",
            "id.token.claim": "true",
            "access.token.claim": "true",
            "userinfo.token.claim": "true"
          }
        },
        {
          "name": "booth-design-audience",
          "protocol": "openid-connect",
          "protocolMapper": "oidc-audience-mapper",
          "config": {
            "included.client.audience": "booth-design",
            "id.token.claim": "false",
            "access.token.claim": "true"
          }
        }
      ]
    }
  ],
  "users": [
    {
      "username": "owner-user",
      "enabled": true, "emailVerified": true, "email": "owner-user@example.test",
      "firstName": "Owner", "lastName": "User",
      "credentials": [{"type": "password", "value": "__TEST_PASSWORD__", "temporary": false}],
      "groups": ["/workspaces/acme-analytics/owner"]
    },
    {
      "username": "editor-user",
      "enabled": true, "emailVerified": true, "email": "editor-user@example.test",
      "firstName": "Editor", "lastName": "User",
      "credentials": [{"type": "password", "value": "__TEST_PASSWORD__", "temporary": false}],
      "groups": ["/workspaces/acme-analytics/editor"]
    },
    {
      "username": "viewer-user",
      "enabled": true, "emailVerified": true, "email": "viewer-user@example.test",
      "firstName": "Viewer", "lastName": "User",
      "credentials": [{"type": "password", "value": "__TEST_PASSWORD__", "temporary": false}],
      "groups": ["/workspaces/acme-analytics/viewer"]
    },
    {
      "username": "outsider-user",
      "enabled": true, "emailVerified": true, "email": "outsider-user@example.test",
      "firstName": "Outsider", "lastName": "User",
      "credentials": [{"type": "password", "value": "__TEST_PASSWORD__", "temporary": false}],
      "groups": ["/workspaces/other-team/owner"]
    }
  ]
}
