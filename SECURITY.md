# Security policy

## Supported versions

Only the latest release gets security fixes. If you're running something older,
the first thing I'll ask is to upgrade.

## Reporting a vulnerability

Please write to
[stefano@dragas.it](mailto:stefano@dragas.it?subject=littleFedi%20security)
and don't open a public issue. Don't put real user data in the report, and
please don't test against instances that aren't yours.

Tell me which version and backend you're running, how to reproduce the
problem and what an attacker could do with it. If you already have an idea for
the fix, even better. If the report is particularly sensitive, send a first
message asking for a public key and I'll reply with one.

I'll try to answer within a few days, and to agree with you on when to publish
the details once a fix is out. This is a small project maintained in spare
time, so sometimes it may take a bit longer, but you won't be left without an
answer. Good-faith research is welcome, as long as it doesn't break anyone's
privacy, take services down or destroy data.

This address is for bugs in littleFedi. Problems with a specific instance
(abuse, moderation, spam) go to that instance's administrators.

## littleMesh trust model

A littleMesh node is named by its own public key, so reaching a node ID means
reaching the holder of that key or failing. A lighthouse relays ciphertext
between two nodes and cannot read, alter, or impersonate them; it can refuse to
carry traffic and it can observe which node IDs talk to each other and when.
littleMesh addresses reachability, not anonymity.

The optional HTTPS gateway is the exception and is opt-in for the relay
operator. It terminates TLS for callers who are not mesh members, so its
operator can read that traffic, tamper with GET responses, and drop requests.
Signed POSTs cannot be altered undetected. Mesh peers never traverse the
gateway. Operators who trust no relay should set `mesh.suffix = "mesh"` and
forgo interoperability with the ordinary fediverse.

The mesh private key in the `mesh_identity` table *is* the instance's identity
on the mesh. Protect and back it up exactly like the ActivityPub actor keys.

The optional directory (the one that gives nodes readable names) doesn't
change this. Every record it serves is signed by the node and checked against
the node ID, and connections still pin the node ID. A compromised directory can
hide a node or lie the first time someone looks up a readable name, but it
can't impersonate a node ID the caller already knows. Back up the directory's
database too.
