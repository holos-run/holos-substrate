package holos

// clusterName is a common parameter for many components, the value is passed
// from the platform/*.cue files to the components when holos render platform
// starts invoking holos render component concurrently.
//
// The default value enables export, for example:
//  holos cue export ./components/gateway-api | jq .holos
clusterName: string | *"k3d-holos" @tag(clusterName)
