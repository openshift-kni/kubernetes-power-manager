{{- define "manager-chart-library.shared-profile" -}}
apiVersion: power.cluster-power-manager.github.io/v1alpha1
kind: PowerProfile
metadata:
  name: {{ .Values.sharedprofile.name }}
  namespace: {{ .Values.sharedprofile.namespace }}
spec:
  max: {{ .Values.sharedprofile.spec.max }}
  min: {{ .Values.sharedprofile.spec.min }}
  epp: {{ .Values.sharedprofile.spec.epp }}
  governor: {{ .Values.sharedprofile.spec.governor }}
{{- end -}}