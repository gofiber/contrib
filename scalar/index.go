package scalar

const templateHTML = `
<!doctype html>
<html>
  <head>
    <title>{{.Title}}</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />

    {{- if .CustomStyle }}
    <style>
      :root {
        {{ .CustomStyle }}
      }
    </style>
    {{ end }}
  </head>

  <body>
    <div id="app"></div>

    <script>
      if (!navigator.onLine) {
        var script = document.createElement('script');
        script.src = '{{ .Extra.FallbackUrl }}';
        script.onload = initScalar;
        document.head.appendChild(script);
      } else {
        var cdn = document.createElement('script');
        cdn.src = 'https://cdn.jsdelivr.net/npm/@scalar/api-reference';
        cdn.onload = initScalar;
        cdn.onerror = function () {
          var fallback = document.createElement('script');
          fallback.src = '{{ .Extra.FallbackUrl }}';
          fallback.onload = initScalar;
          document.head.appendChild(fallback);
        };
        document.head.appendChild(cdn);
      }

      function initScalar() {
        if (typeof Scalar !== 'undefined') {
          Scalar.createApiReference('#app', {
            content: {{.FileContentString}},
            {{- if .ProxyUrl}}
            proxyUrl: "{{.ProxyUrl}}",
            {{ end }}
          });
        }
      }
    </script>
  </body>
</html>`
