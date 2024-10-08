# Data

**Contents**

<!-- toc -->

See also:

- [Data store](store.md)
- [Data syncronization](sync.md)

## Data Structures

As a client developer, there are two main primary structures:
[`NodeEdge`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#NodeEdge)
and [`Point`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#Point). A
`Node` can be considered a collection of `Points`.

These data structures describe most data that is stored and transferred in a
Simple IoT system.

The core data structures are currently defined in the
[`data`](https://github.com/simpleiot/simpleiot/tree/master/data) directory for
Go code, and
[`frontend/src/Api`](https://github.com/simpleiot/simpleiot/tree/master/frontend/src/Api)
directory for Elm code.

A `Point` can represent a sensor value, or a configuration parameter for the
node. With sensor values and configuration represented as `Points`, it becomes
easy to use both sensor data and configuration in rule or equations because the
mechanism to use both is the same. Additionally, if all `Point` changes are
recorded in a time series database (for instance Influxdb), you automatically
have a record of all configuration and sensor changes for a `node`.

Treating most data as `Points` also has another benefit in that we can easily
simulate a device -- simply provide a UI or write a program to modify any point
and we can shift from working on real data to simulating scenarios we want to
test.

Edges are used to describe the relationships between nodes as a
[directed acyclic graph](https://en.wikipedia.org/wiki/Directed_acyclic_graph).

![dag](images/dag.svg)

`Nodes` can have parents or children and thus be represented in a hierarchy. To
add structure to the system, you simply add nested `Nodes`. The `Node` hierarchy
can represent the physical structure of the system, or it could also contain
virtual `Nodes`. These virtual nodes could contain logic to process data from
sensors. Several examples of virtual nodes:

- a pump `Node` that converts motor current readings into pump events.
- implement moving averages, scaling, etc on sensor data.
- combine data from multiple sensors
- implement custom logic for a particular application
- a component in an edge device such as a cellular modem

Like Nodes, Edges also contain a Point array that further describes the
relationship between Nodes. Some examples:

- role the user plays in the node (viewer, admin, etc)
- order of notifications when sequencing notifications through a node's users
- node is enabled/disabled -- for instance we may want to disable a Modbus IO
  node that is not currently functioning.

Being able to arranged nodes in an arbitrary hierarchy also opens up some
interesting possibilities such as creating virtual nodes that have a number of
children that are collecting data. The parent virtual nodes could have rules or
logic that operate off data from child nodes. In this case, the virtual parent
nodes might be a town or city, service provider, etc., and the child nodes are
physical edge nodes collecting data, users, etc.

### The Point `Key` field constraint

The Point data structure has a `Key` field that can be used to construct Array
and Map data structures in a node. This is a flexible idea in that it is easy to
transition from a scaler value to an array or map. However, it can also cause
problems if one client is writing key values of `""` and another client (say a
rule action) is writing value of `"0"`. One solution is to have fancy logic that
equates `""` to `"0"` on point updates, compares, etc. Another approach is to
consider `""` and invalid key value and set key to `"0"` for scaler values. This
incurs a slight amount of overhead, but leads to more predictable operation and
eliminates the possibility of having two points in a node that mean the same
things.

**The Simple IoT Store always sets the Key field to `"0"` on incoming points if
the Key field is blank.**

Clients should be written with this in mind.

### Converting Nodes to other data structures

Nodes and Points are convenient for storage and synchronization, but cumbersome
to work with in application code that uses the data, so we typically convert
them to another data structure.
[`data.Decode`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#Decode),
[`data.Encode`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#Encode),
and
[`data.MergePoints`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#MergePoints)
can be used to convert Node data structures to your own custom `struct`, much
like the Go `json` package.

### Arrays and Maps

Points can be used to represent arrays and maps. For an array, the `key` field
contains the index `"0"`, `"1"`, `"2"`, etc. For maps, the `key` field contains
the key of the map. An example:

| Type            | Key   | Text             | Value |
| --------------- | ----- | ---------------- | ----- |
| description     | 0     | Node Description |       |
| ipAddress       | 0     | 192.168.1.10     |       |
| ipAddress       | 1     | 10.0.0.3         |       |
| diskPercentUsed | /     |                  | 43    |
| diskPercentUsed | /home |                  | 75    |
| switch          | 0     |                  | 1     |
| switch          | 1     |                  | 0     |

The above would map to the following Go type:

```go
type myNode struct {
    ID              string      `node:"id"`
    Parent          string      `node:"parent"`
    Description     string      `node:"description"`
    IpAddresses     []string    `point:"ipAddress"`
    Switches        []bool      `point:"switch"`
    DiscPercentUsed []float64   `point:"diskPercentUsed"`
}
```

The
[`data.Decode()`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#Decode)
function can be used to decode an array of points into the above type. The
[`data.Merge()`](https://pkg.go.dev/github.com/simpleiot/simpleiot/data#MergePoints)
function can be used to update an existing struct from a new point.

#### Best practices for working with arrays

If you are going to make changes to an array in UI/Client code, and you are
storing the array in a native structure, then you also need to store a length
field as well so you know how long the original array was. After modifying the
array, check if the new length is less than the original -- if it is, then add a
tombstone points to the end so that the deleted points get removed.

Generally it is simplest to send the entire array as a single message any time
any value in it has changed -- especially if values are going to be added or
removed. The `data.Decode` will then correctly handle the array resizing.

#### Technical details of how `data.Decode` works with slices

Some consideration is needed when using `Decode` and `MergePoints` to decode
points into Go slices. Slices are never allocated / copied unless they are being
expanded. Instead, deleted points are written to the slice as the zero value.
However, for a given `Decode` call, if points are deleted from the _end_ of the
slice, `Decode` will re-slice it to remove those values from the slice. Thus,
there is an important consideration for clients: if they wish to rely on slices
being truncated when points are deleted, points must be batched in order such
that `Decode` sees the trailing deleted points first. Put another way, `Decode`
does not care about points deleted from prior calls to `Decode`, so "holes" of
zero values may still appear at the end of a slice under certain circumstances.
Consider points with integer values `[0, 1, 2, 3, 4]`. If tombstone is set on
point with `Key` 3 followed by a point tombstone set on point with `Key` `4`,
the resulting slice will be `[0, 1, 2]` if these points are batched together,
but if they are sent separately (thus resulting in multiple `Decode` calls), the
resulting slice will be `[0, 1, 2, 0]`.

## Node Topology changes

Nodes can exist in multiple locations in the tree. This allows us to do things
like include a user in multiple groups.

### Add

Node additions are detected in real-time by sending the points for the new node
as well as points for the edge node that adds the node to the tree.

### Copy

Node copies are are similar to add, but only the edge points are sent.

### Delete

Node deletions are recorded by setting a tombstone point in the edge above the
node to true. If a node is deleted, this information needs to be recorded,
otherwise the synchronization process will simply re-create the deleted node if
it exists on another instance.

### Move

Move is just a combination of Copy and Delete.

If the any real-time data is lost in any of the above operations, the catch up
synchronization will propagate any node changes.

## Tracking who made changes

The `Point` type has an `Origin` field that is used to track who generated this
point. If the node that owned the point generated the point, then Origin can be
left blank -- this saves data bandwidth -- especially for sensor data which is
generated by the client managing the node. There are several reasons for the
`Origin` field:

- track who made changes for auditing and debugging purposes. If a rule or some
  process other than the owning node modifies a point, the Origin should always
  be populated. Tests that generate points should generally set the origin to
  "test".
- eliminate echos where a client may be subscribed to a subject as well as
  publish to the same subject. With the Origin field, the client can determine
  if it was the author of a point it receives, and if so simply drop it. See
  [client documentation](client.md#message-echo) for more discussion of the echo
  topic.

## Evolvability

One important consideration in data design is the can the system be easily
changed. With a distributed system, you may have different versions of the
software running at the same time using the same data. One version may use/store
additional information that the other does not. In this case, it is very
important that the other version does not delete this data, as could easily
happen if you decode data into a type, and then re-encode and store it.

With the Node/Point system, we don't have to worry about this issue because
Nodes are only updated by sending Points. It is not possible to delete a Node
Point. So it one version writes a Point the other is not using, it will be
transferred, stored, synchronized, etc and simply ignored by version that don't
use this point. This is another case where SIOT solves a hard problem that
typically requires quite a bit of care and effort.
