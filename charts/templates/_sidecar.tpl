{{- define "w7panel-cloudnoauth.hostAliases" -}}
- ip: {{ .Values.sidecar.virtualIP | quote }}
  hostnames:
    - {{ .Values.sidecar.targetHost | quote }}
{{- end -}}

{{- define "w7panel-cloudnoauth.initContainer" -}}
- name: w7panel-cloudnoauth-iptables
  image: "{{ .Values.sidecar.image.repository }}:{{ .Values.sidecar.image.tag | default .Chart.AppVersion }}"
  imagePullPolicy: {{ .Values.sidecar.image.pullPolicy }}
  command: ["/usr/local/bin/iptables-setup"]
  securityContext:
    runAsUser: 0
    runAsGroup: 0
    runAsNonRoot: false
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: false
    capabilities:
      add: ["NET_ADMIN"]
      drop: ["ALL"]
  env:
    - name: API_PROXY_VIRTUAL_IP
      value: {{ .Values.sidecar.virtualIP | quote }}
    - name: PROXY_HTTP_PORT
      value: {{ .Values.sidecar.httpPort | quote }}
    - name: PROXY_HTTPS_PORT
      value: {{ .Values.sidecar.httpsPort | quote }}
    - name: SIDECAR_RUNTIME_UID
      value: {{ .Values.sidecar.runtimeUID | quote }}
{{- end -}}

{{- define "w7panel-cloudnoauth.container" -}}
- name: w7panel-cloudnoauth
  image: "{{ .Values.sidecar.image.repository }}:{{ .Values.sidecar.image.tag | default .Chart.AppVersion }}"
  imagePullPolicy: {{ .Values.sidecar.image.pullPolicy }}
  securityContext:
    runAsUser: {{ .Values.sidecar.runtimeUID }}
    runAsGroup: {{ .Values.sidecar.runtimeUID }}
    runAsNonRoot: true
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    capabilities:
      drop: ["ALL"]
  ports:
    - name: noauth-http
      containerPort: {{ .Values.sidecar.httpPort }}
      protocol: TCP
    - name: noauth-https
      containerPort: {{ .Values.sidecar.httpsPort }}
      protocol: TCP
  env:
    - name: SERVER_PORT
      value: {{ .Values.sidecar.httpPort | quote }}
    - name: SERVER_TLS_PORT
      value: {{ .Values.sidecar.httpsPort | quote }}
    - name: SERVER_TLS_CERT_FILE
      value: {{ printf "%s/tls.crt" (.Values.sidecar.tls.mountPath | trimSuffix "/") | quote }}
    - name: SERVER_TLS_KEY_FILE
      value: {{ printf "%s/tls.key" (.Values.sidecar.tls.mountPath | trimSuffix "/") | quote }}
    - name: API_PROXY_ALLOWED_HOST
      value: {{ .Values.sidecar.targetHost | quote }}
    - name: API_PROXY_UPSTREAM_HOST
      value: {{ include "w7panel-cloudnoauth.upstreamServiceHost" . | quote }}
    - name: API_PROXY_UPSTREAM_CA_FILE
      value: {{ .Values.sidecar.upstream.caFile | quote }}
    - name: POD_NAME
      valueFrom:
        fieldRef:
          fieldPath: metadata.name
    - name: PANEL_NAMESPACE
      {{- if .Values.sidecar.panelNamespace }}
      value: {{ .Values.sidecar.panelNamespace | quote }}
      {{- else }}
      valueFrom:
        fieldRef:
          fieldPath: metadata.namespace
      {{- end }}
    {{- range $name, $value := .Values.sidecar.env }}
    - name: {{ $name }}
      value: {{ $value | quote }}
    {{- end }}
    {{- with .Values.sidecar.extraEnv }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  volumeMounts:
    - name: {{ .Values.sidecar.tls.volumeName }}
      mountPath: {{ .Values.sidecar.tls.mountPath }}
      readOnly: true
  livenessProbe:
    httpGet:
      path: /api/live
      port: noauth-http
    initialDelaySeconds: 5
    periodSeconds: 10
  readinessProbe:
    httpGet:
      path: /api/live
      port: noauth-http
    initialDelaySeconds: 2
    periodSeconds: 5
  {{- with .Values.sidecar.resources }}
  resources:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}

{{- define "w7panel-cloudnoauth.jobContainer" -}}
{{- $containers := include "w7panel-cloudnoauth.container" . | fromYamlArray -}}
{{- $_ := set (index $containers 0) "restartPolicy" "Always" -}}
{{ toYaml $containers }}
{{- end -}}

{{- define "w7panel-cloudnoauth.volumes" -}}
- name: {{ .Values.sidecar.tls.volumeName }}
  csi:
    driver: csi.cert-manager.io
    readOnly: true
    volumeAttributes:
      csi.cert-manager.io/issuer-name: {{ .Values.sidecar.issuer.name | quote }}
      csi.cert-manager.io/issuer-kind: {{ .Values.sidecar.issuer.kind | quote }}
      csi.cert-manager.io/issuer-group: {{ .Values.sidecar.issuer.group | quote }}
      csi.cert-manager.io/common-name: {{ .Values.sidecar.targetHost | quote }}
      csi.cert-manager.io/dns-names: {{ .Values.sidecar.targetHost | quote }}
      csi.cert-manager.io/fs-group: {{ .Values.sidecar.runtimeUID | quote }}
{{- end -}}

{{- define "w7panel-cloudnoauth.resources" -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "w7panel-cloudnoauth.upstreamServiceName" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "w7panel-cloudnoauth.labels" . | nindent 4 }}
spec:
  type: ExternalName
  externalName: {{ .Values.sidecar.targetHost | quote }}
{{- if ne .Values.sidecar.serviceAccountName "default" }}
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: {{ .Values.sidecar.serviceAccountName }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "w7panel-cloudnoauth.labels" . | nindent 4 }}
{{- end }}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "w7panel-cloudnoauth.fullname" . }}
  namespace: {{ .Release.Namespace }}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["replicasets", "deployments"]
    verbs: ["get"]
  - apiGroups: ["w7panel.w7.com"]
    resources: ["appgroups"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "w7panel-cloudnoauth.fullname" . }}
  namespace: {{ .Release.Namespace }}
subjects:
  - kind: ServiceAccount
    name: {{ .Values.sidecar.serviceAccountName }}
    namespace: {{ .Release.Namespace }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "w7panel-cloudnoauth.fullname" . }}
{{- end -}}
