{ config, ... }:

{
  services.nginx.virtualHosts."lab.cloonar.com" = {
    forceSSL = true;
    enableACME = true;
    acmeRoot = null;

    extraConfig = ''
      # Virtual endpoint created by nginx to forward auth_request checks to
      # Authelia (running on web-arm).
      location /authelia {
        internal;
        # Modern Authelia (4.38+) authz endpoint. Hits the unrestricted `/`
        # vhost on web-arm (the legacy /api/verify location has an IP
        # allowlist that excludes fw's current WAN IP).
        proxy_pass https://auth.cloonar.com/api/authz/auth-request;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";

        proxy_next_upstream error timeout invalid_header http_500 http_502 http_503;

        client_body_buffer_size 128k;
        # Must be auth.cloonar.com so web-arm's nginx matches the right
        # vhost; passing $host would send Host: lab.cloonar.com and miss
        # every server_name on web-arm (→ 404 → auth_request errors → 500).
        proxy_set_header Host auth.cloonar.com;
        proxy_set_header X-Original-Method $request_method;
        proxy_set_header X-Original-URL $scheme://$http_host$request_uri;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $http_host;
        proxy_set_header X-Forwarded-Uri $request_uri;
        proxy_set_header X-Forwarded-Ssl on;
        proxy_redirect  http://  $scheme://;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_cache_bypass $cookie_session;
        proxy_no_cache $cookie_session;
        proxy_buffers 4 32k;

        send_timeout 5m;
        proxy_read_timeout 240;
        proxy_send_timeout 240;
        proxy_connect_timeout 240;
      }
    '';

    locations."/" = {
      proxyPass = "http://${config.networkPrefix}.97.15:8080";
      extraConfig = ''
        # Gate every request through Authelia. The existing *.cloonar.com rule
        # in hosts/web-arm/modules/authelia.nix requires two_factor for
        # group:Administrators or group:Mitarbeiter — effectively cloonar.com
        # staff, since those groups live in the dc=cloonar,dc=com LDAP tenant.
        auth_request /authelia;
        auth_request_set $target_url $scheme://$http_host$request_uri;
        error_page 401 =302 https://auth.cloonar.com/?rd=$target_url;
      '';
    };
  };
}
