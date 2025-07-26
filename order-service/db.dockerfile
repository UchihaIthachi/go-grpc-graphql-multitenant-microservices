FROM cassandra:3.11

ADD up.cql /
RUN until cqlsh -f /up.cql; do echo "cqlsh: Cassandra is not ready yet - sleeping"; sleep 1; done &

CMD ["cassandra", "-f"]
