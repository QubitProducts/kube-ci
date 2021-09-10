// package: kubeci
// file: kubeci.proto

var kubeci_pb = require("./kubeci_pb");
var grpc = require("@improbable-eng/grpc-web").grpc;

var KubeCI = (function () {
  function KubeCI() {}
  KubeCI.serviceName = "kubeci.KubeCI";
  return KubeCI;
}());

KubeCI.ListTemplate = {
  methodName: "ListTemplate",
  service: KubeCI,
  requestStream: false,
  responseStream: false,
  requestType: kubeci_pb.ListTemplatesRequest,
  responseType: kubeci_pb.ListTemplatesResponse
};

KubeCI.SetupRepo = {
  methodName: "SetupRepo",
  service: KubeCI,
  requestStream: false,
  responseStream: false,
  requestType: kubeci_pb.SetupRepoRequest,
  responseType: kubeci_pb.SetupRepoResponse
};

exports.KubeCI = KubeCI;

function KubeCIClient(serviceHost, options) {
  this.serviceHost = serviceHost;
  this.options = options || {};
}

KubeCIClient.prototype.listTemplate = function listTemplate(requestMessage, metadata, callback) {
  if (arguments.length === 2) {
    callback = arguments[1];
  }
  var client = grpc.unary(KubeCI.ListTemplate, {
    request: requestMessage,
    host: this.serviceHost,
    metadata: metadata,
    transport: this.options.transport,
    debug: this.options.debug,
    onEnd: function (response) {
      if (callback) {
        if (response.status !== grpc.Code.OK) {
          var err = new Error(response.statusMessage);
          err.code = response.status;
          err.metadata = response.trailers;
          callback(err, null);
        } else {
          callback(null, response.message);
        }
      }
    }
  });
  return {
    cancel: function () {
      callback = null;
      client.close();
    }
  };
};

KubeCIClient.prototype.setupRepo = function setupRepo(requestMessage, metadata, callback) {
  if (arguments.length === 2) {
    callback = arguments[1];
  }
  var client = grpc.unary(KubeCI.SetupRepo, {
    request: requestMessage,
    host: this.serviceHost,
    metadata: metadata,
    transport: this.options.transport,
    debug: this.options.debug,
    onEnd: function (response) {
      if (callback) {
        if (response.status !== grpc.Code.OK) {
          var err = new Error(response.statusMessage);
          err.code = response.status;
          err.metadata = response.trailers;
          callback(err, null);
        } else {
          callback(null, response.message);
        }
      }
    }
  });
  return {
    cancel: function () {
      callback = null;
      client.close();
    }
  };
};

exports.KubeCIClient = KubeCIClient;

