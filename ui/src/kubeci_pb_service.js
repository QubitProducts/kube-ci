// package: kubecipb
// file: kubeci.proto

var kubeci_pb = require("./kubeci_pb");
var grpc = require("@improbable-eng/grpc-web").grpc;

var CIRunner = (function () {
  function CIRunner() {}
  CIRunner.serviceName = "kubecipb.CIRunner";
  return CIRunner;
}());

exports.CIRunner = CIRunner;

function CIRunnerClient(serviceHost, options) {
  this.serviceHost = serviceHost;
  this.options = options || {};
}

exports.CIRunnerClient = CIRunnerClient;

