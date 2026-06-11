package holos

// clusterName is a common parameter for many components, the value is passed
// from the platform/*.cue files to the components when holos render platform
// starts invoking holos render component concurrently.
//
// The default value enables export, for example:
//  holos cue export ./components/gateway-api | jq .holos
//
// Keep the default in sync with the local development cluster registered in
// platform/platform.cue.  Rendering is unaffected by drift (the tag is always
// injected by holos render platform); only cue export defaults would diverge.
clusterName: string | *"k3d-holos" @tag(clusterName)
